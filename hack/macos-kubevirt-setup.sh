#!/usr/bin/env bash
#
# macOS KubeVirt setup script for Apple Silicon (M-series) with Podman + libkrun.
#
# This script:
#   1. Validates prerequisites
#   2. Brings up a kind-based k8s cluster via kubevirtci
#   3. Deploys KubeVirt from an official release
#   4. Enables software emulation (required on macOS – no /dev/kvm in kind nodes)
#   5. Applies cluster-level workarounds for aarch64 TCG:
#      a) Patches the ValidatingWebhookConfiguration to allow named CPU models on ARM64
#      b) Adds the cpu-model.node.kubevirt.io/cortex-a57=true node label so
#         virt-launcher pods can schedule (virt-handler skips TCG CPU model discovery
#         when KVM is absent)
#   6. Waits until the stack is fully ready
#   7. Deploys examples/vmi-arm.yaml as a smoke-test VM
#
# Known macOS + aarch64 TCG issues addressed here:
#   - host-passthrough CPU mode not supported by QEMU TCG on aarch64 → use cortex-a57
#   - KubeVirt validation webhook blocks non-passthrough CPU models → patched
#   - virt-handler does not discover TCG CPU models → node label applied manually
#   - useEmulation required because kind nodes have no /dev/kvm
#
# Usage:
#   export KUBEVIRT_VERSION=v1.8.3   # optional, defaults to v1.8.3
#   ./hack/macos-kubevirt-setup.sh
#
# To tear down afterwards:
#   make cluster-down    (or: KUBEVIRT_PROVIDER=kind-1.35 ./kubevirtci/cluster-up/down.sh)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

KUBEVIRT_VERSION="${KUBEVIRT_VERSION:-v1.8.3}"
KUBEVIRT_PROVIDER="${KUBEVIRT_PROVIDER:-kind-1.35}"
KUBEVIRT_RELEASE_BASE="https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}"

# ARM64 TCG fallback CPU model – cortex-a57 is universally supported by QEMU TCG on aarch64
AARCH64_TCG_CPU_MODEL="cortex-a57"

KUBEVIRTCI_CONFIG_PATH="${REPO_ROOT}/kubevirtci/_ci-configs"
KUBECONFIG_PATH="${KUBEVIRTCI_CONFIG_PATH}/${KUBEVIRT_PROVIDER}/.kubeconfig"
KUBECTL_WRAPPER="${KUBEVIRTCI_CONFIG_PATH}/${KUBEVIRT_PROVIDER}/.kubectl"

# ── colour helpers ──────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info()    { echo -e "${BLUE}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()     { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

# ── 0. guard: macOS only ────────────────────────────────────────────────────
[[ "$(uname -s)" == "Darwin" ]] || die "This script is intended for macOS only."

echo ""
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║         KubeVirt macOS Setup (Apple Silicon + Podman)        ║"
echo "║                                                              ║"
echo "║  Provider : ${KUBEVIRT_PROVIDER}                                    ║"
echo "║  KubeVirt : ${KUBEVIRT_VERSION}                                        ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""

# ── 1. prerequisites check ──────────────────────────────────────────────────
info "Checking prerequisites..."

check_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "Required command not found: $1. Please install it and retry."
}

check_cmd podman
check_cmd kubectl
check_cmd curl
check_cmd awk

# Podman machine must be running
if ! podman machine list 2>/dev/null | grep -q "Currently running"; then
    warn "No running Podman machine detected. Starting podman-machine-default..."
    podman machine start podman-machine-default || die "Failed to start Podman machine."
fi

# Confirm Podman is reachable
podman ps >/dev/null 2>&1 || die "Podman is not responding. Check 'podman machine list' and 'podman info'."

success "All prerequisites satisfied."

# ── 2. environment ──────────────────────────────────────────────────────────
export KUBEVIRT_PROVIDER
export CRI_BIN=podman
export KIND_EXPERIMENTAL_PROVIDER=podman
# Disable etcd-in-memory on macOS (Podman VM containers don't expose tmpfs at /tmp)
export KUBEVIRT_WITH_KIND_ETCD_IN_MEMORY=false
# Do NOT deploy CDI by default to save time; set to true if needed
export KUBEVIRT_DEPLOY_CDI="${KUBEVIRT_DEPLOY_CDI:-false}"

cd "${REPO_ROOT}"

# ── 3. cluster-up ───────────────────────────────────────────────────────────
info "Starting kind cluster '${KUBEVIRT_PROVIDER}'..."
info "  → This typically takes 3–6 minutes on first run."

make cluster-up

# cluster-up writes .kubectl and .kubeconfig; verify they exist
[[ -f "${KUBECTL_WRAPPER}" ]]  || die ".kubectl wrapper not found after cluster-up: ${KUBECTL_WRAPPER}"
[[ -f "${KUBECONFIG_PATH}" ]]  || die ".kubeconfig not found after cluster-up: ${KUBECONFIG_PATH}"

success "Cluster is up."

_kubectl() { "${KUBECTL_WRAPPER}" --kubeconfig="${KUBECONFIG_PATH}" "$@"; }

info "Cluster nodes:"
_kubectl get nodes -o wide

# ── 4. deploy KubeVirt operator ─────────────────────────────────────────────
info "Deploying KubeVirt ${KUBEVIRT_VERSION} operator..."
_kubectl apply -f "${KUBEVIRT_RELEASE_BASE}/kubevirt-operator.yaml"

info "Deploying KubeVirt CR..."
_kubectl apply -f "${KUBEVIRT_RELEASE_BASE}/kubevirt-cr.yaml"

# ── 5. enable software emulation (no /dev/kvm on macOS kind nodes) ──────────
info "Enabling useEmulation (required on macOS – no hardware KVM in kind containers)..."

# Wait until the KubeVirt CR exists before patching
for i in $(seq 1 30); do
    if _kubectl -n kubevirt get kubevirt kubevirt &>/dev/null; then
        break
    fi
    echo "  Waiting for KubeVirt CR to become available... (${i}/30)"
    sleep 10
done

_kubectl -n kubevirt patch kubevirt kubevirt --type=merge \
    -p '{"spec":{"configuration":{"developerConfiguration":{"useEmulation":true}}}}'

success "useEmulation enabled."

# ── 6. wait for KubeVirt to be ready ────────────────────────────────────────
info "Waiting for KubeVirt to be fully deployed (this may take 5–10 minutes)..."

_kubectl -n kubevirt wait kubevirt kubevirt \
    --for=condition=Available \
    --timeout=600s

success "KubeVirt is ready."

info "KubeVirt pods:"
_kubectl -n kubevirt get pods -o wide

# ── 7. aarch64 TCG workarounds ──────────────────────────────────────────────
#
# Problem A: KubeVirt validation webhook rejects any CPU model except
#   host-passthrough on ARM64. However, host-passthrough requires KVM and is
#   rejected by QEMU TCG on aarch64. We allow named models (e.g. cortex-a57)
#   by patching the namespaceSelector on the VMI create/update validators so
#   they do not apply to the default namespace.
#
# Problem B: virt-handler does not discover QEMU TCG CPU models when KVM is
#   absent. The virt-launcher pod nodeSelector requires
#   cpu-model.node.kubevirt.io/<model>=true on the node. We add the label
#   manually.
#
# Both workarounds are needed until KubeVirt upstream supports TCG on aarch64
# natively (tracked in pkg/virt-api/webhooks/arm64.go and
# pkg/virt-launcher/virtwrap/converter/kvm/configurator.go).
#
info "Applying aarch64 TCG workarounds..."

# Workaround A: patch VMI create/update validators to skip the default namespace
info "  [A] Patching VMI validators to allow named CPU models in the default namespace..."
_kubectl patch validatingwebhookconfiguration virt-api-validator \
    --type='json' \
    -p='[{"op":"add","path":"/webhooks/2/namespaceSelector","value":{"matchLabels":{"kubevirt.io/vmi-strict-validation":"enabled"}}}]' 2>&1 || warn "Failed to patch create-validator (may already be patched)"

_kubectl patch validatingwebhookconfiguration virt-api-validator \
    --type='json' \
    -p='[{"op":"add","path":"/webhooks/3/namespaceSelector","value":{"matchLabels":{"kubevirt.io/vmi-strict-validation":"enabled"}}}]' 2>&1 || warn "Failed to patch update-validator (may already be patched)"

# Workaround B: label the node with the cortex-a57 CPU model so virt-launcher pods schedule
NODE_NAME=$(_kubectl get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
info "  [B] Adding ${AARCH64_TCG_CPU_MODEL} CPU model label to node '${NODE_NAME}'..."
_kubectl label node "${NODE_NAME}" \
    "cpu-model.node.kubevirt.io/${AARCH64_TCG_CPU_MODEL}=true" \
    --overwrite 2>&1

success "aarch64 TCG workarounds applied."

# ── 8. deploy the smoke-test VMI ─────────────────────────────────────────────
info "Deploying examples/vmi-arm.yaml (cpu.model=${AARCH64_TCG_CPU_MODEL})..."
_kubectl apply -f "${REPO_ROOT}/examples/vmi-arm.yaml"

info "Waiting for VMI 'vmi-arm' to be in Running phase (up to 10 minutes)..."
for i in $(seq 1 60); do
    PHASE=$(_kubectl get vmi vmi-arm -o jsonpath='{.status.phase}' 2>/dev/null || echo "Pending")
    echo "  VMI phase: ${PHASE} (${i}/60)"
    if [[ "${PHASE}" == "Running" ]]; then
        break
    elif [[ "${PHASE}" == "Failed" ]]; then
        echo ""
        warn "VMI entered Failed phase. Gathering diagnostics..."
        _kubectl describe vmi vmi-arm || true
        _kubectl -n kubevirt logs -l kubevirt.io=virt-launcher --tail=50 2>/dev/null || true
        die "VMI failed to start. See diagnostics above."
    fi
    sleep 10
done

FINAL_PHASE=$(_kubectl get vmi vmi-arm -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
if [[ "${FINAL_PHASE}" != "Running" ]]; then
    warn "VMI did not reach Running phase within the timeout. Current phase: ${FINAL_PHASE}"
    _kubectl describe vmi vmi-arm || true
else
    success "VMI 'vmi-arm' is Running!"
fi

# ── 9. summary ───────────────────────────────────────────────────────────────
echo ""
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║                        SETUP COMPLETE                       ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""
info "Useful commands:"
echo "  # Show VMI status"
echo "  ${KUBECTL_WRAPPER} --kubeconfig=${KUBECONFIG_PATH} get vmi vmi-arm"
echo ""
echo "  # Connect via serial console (login: cirros / gocubsgo)"
echo "  virtctl --kubeconfig=${KUBECONFIG_PATH} console vmi-arm"
echo ""
echo "  # Connect via VNC"
echo "  virtctl --kubeconfig=${KUBECONFIG_PATH} vnc vmi-arm"
echo ""
echo "  # Watch all events"
echo "  ${KUBECTL_WRAPPER} --kubeconfig=${KUBECONFIG_PATH} get events --sort-by='.lastTimestamp'"
echo ""
echo "  # Tear down cluster"
echo "  KUBEVIRT_PROVIDER=${KUBEVIRT_PROVIDER} make cluster-down"
echo ""
#
# This script:
#   1. Validates prerequisites
#   2. Brings up a kind-based k8s cluster via kubevirtci
#   3. Deploys KubeVirt from an official release
#   4. Enables software emulation (required on macOS – no /dev/kvm in kind nodes)
#   5. Waits until the stack is fully ready
#   6. Deploys examples/vmi-arm.yaml as a smoke-test VM
#
# Usage:
#   export KUBEVIRT_VERSION=v1.8.3   # optional, defaults to v1.8.3
#   ./hack/macos-kubevirt-setup.sh
#
# To tear down afterwards:
#   make cluster-down    (or: KUBEVIRT_PROVIDER=kind-1.35 ./kubevirtci/cluster-up/down.sh)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

KUBEVIRT_VERSION="${KUBEVIRT_VERSION:-v1.8.3}"
KUBEVIRT_PROVIDER="${KUBEVIRT_PROVIDER:-kind-1.35}"
KUBEVIRT_RELEASE_BASE="https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}"

KUBEVIRTCI_CONFIG_PATH="${REPO_ROOT}/kubevirtci/_ci-configs"
KUBECONFIG_PATH="${KUBEVIRTCI_CONFIG_PATH}/${KUBEVIRT_PROVIDER}/.kubeconfig"
KUBECTL_WRAPPER="${KUBEVIRTCI_CONFIG_PATH}/${KUBEVIRT_PROVIDER}/.kubectl"

# ── colour helpers ──────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info()    { echo -e "${BLUE}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()     { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

# ── 0. guard: macOS only ────────────────────────────────────────────────────
[[ "$(uname -s)" == "Darwin" ]] || die "This script is intended for macOS only."

echo ""
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║         KubeVirt macOS Setup (Apple Silicon + Podman)        ║"
echo "║                                                              ║"
echo "║  Provider : ${KUBEVIRT_PROVIDER}                                    ║"
echo "║  KubeVirt : ${KUBEVIRT_VERSION}                                        ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""

# ── 1. prerequisites check ──────────────────────────────────────────────────
info "Checking prerequisites..."

check_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "Required command not found: $1. Please install it and retry."
}

check_cmd podman
check_cmd kubectl
check_cmd curl
check_cmd awk

# Podman machine must be running
if ! podman machine list 2>/dev/null | grep -q "Currently running"; then
    warn "No running Podman machine detected. Starting podman-machine-default..."
    podman machine start podman-machine-default || die "Failed to start Podman machine."
fi

# Confirm Podman is reachable
podman ps >/dev/null 2>&1 || die "Podman is not responding. Check 'podman machine list' and 'podman info'."

success "All prerequisites satisfied."

# ── 2. environment ──────────────────────────────────────────────────────────
export KUBEVIRT_PROVIDER
export CRI_BIN=podman
export KIND_EXPERIMENTAL_PROVIDER=podman
# Disable etcd-in-memory on macOS (Podman VM containers don't expose tmpfs at /tmp)
export KUBEVIRT_WITH_KIND_ETCD_IN_MEMORY=false
# Do NOT deploy CDI by default to save time; set to true if needed
export KUBEVIRT_DEPLOY_CDI="${KUBEVIRT_DEPLOY_CDI:-false}"

cd "${REPO_ROOT}"

# ── 3. cluster-up ───────────────────────────────────────────────────────────
info "Starting kind cluster '${KUBEVIRT_PROVIDER}'..."
info "  → This typically takes 3–6 minutes on first run."

make cluster-up

# cluster-up writes .kubectl and .kubeconfig; verify they exist
[[ -f "${KUBECTL_WRAPPER}" ]]  || die ".kubectl wrapper not found after cluster-up: ${KUBECTL_WRAPPER}"
[[ -f "${KUBECONFIG_PATH}" ]]  || die ".kubeconfig not found after cluster-up: ${KUBECONFIG_PATH}"

success "Cluster is up."

_kubectl() { "${KUBECTL_WRAPPER}" --kubeconfig="${KUBECONFIG_PATH}" "$@"; }

info "Cluster nodes:"
_kubectl get nodes -o wide

# ── 4. deploy KubeVirt operator ─────────────────────────────────────────────
info "Deploying KubeVirt ${KUBEVIRT_VERSION} operator..."
_kubectl apply -f "${KUBEVIRT_RELEASE_BASE}/kubevirt-operator.yaml"

info "Deploying KubeVirt CR..."
_kubectl apply -f "${KUBEVIRT_RELEASE_BASE}/kubevirt-cr.yaml"

# ── 5. enable software emulation (no /dev/kvm on macOS kind nodes) ──────────
info "Enabling useEmulation (required on macOS – no hardware KVM in kind containers)..."

# Wait until the KubeVirt CR exists before patching
for i in $(seq 1 30); do
    if _kubectl -n kubevirt get kubevirt kubevirt &>/dev/null; then
        break
    fi
    echo "  Waiting for KubeVirt CR to become available... (${i}/30)"
    sleep 10
done

_kubectl -n kubevirt patch kubevirt kubevirt --type=merge \
    -p '{"spec":{"configuration":{"developerConfiguration":{"useEmulation":true}}}}'

success "useEmulation enabled."

# ── 6. wait for KubeVirt to be ready ────────────────────────────────────────
info "Waiting for KubeVirt to be fully deployed (this may take 5–10 minutes)..."

_kubectl -n kubevirt wait kubevirt kubevirt \
    --for=condition=Available \
    --timeout=600s

success "KubeVirt is ready."

info "KubeVirt pods:"
_kubectl -n kubevirt get pods -o wide

# ── 7. deploy the smoke-test VMI ─────────────────────────────────────────────
info "Deploying examples/vmi-arm.yaml..."
_kubectl apply -f "${REPO_ROOT}/examples/vmi-arm.yaml"

info "Waiting for VMI 'vmi-arm' to be in Running phase (up to 10 minutes)..."
for i in $(seq 1 60); do
    PHASE=$(_kubectl get vmi vmi-arm -o jsonpath='{.status.phase}' 2>/dev/null || echo "Pending")
    echo "  VMI phase: ${PHASE} (${i}/60)"
    if [[ "${PHASE}" == "Running" ]]; then
        break
    elif [[ "${PHASE}" == "Failed" ]]; then
        echo ""
        warn "VMI entered Failed phase. Gathering diagnostics..."
        _kubectl describe vmi vmi-arm || true
        _kubectl -n kubevirt logs -l kubevirt.io=virt-launcher --tail=50 2>/dev/null || true
        die "VMI failed to start. See diagnostics above."
    fi
    sleep 10
done

FINAL_PHASE=$(_kubectl get vmi vmi-arm -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
if [[ "${FINAL_PHASE}" != "Running" ]]; then
    warn "VMI did not reach Running phase within the timeout. Current phase: ${FINAL_PHASE}"
    _kubectl describe vmi vmi-arm || true
else
    success "VMI 'vmi-arm' is Running!"
fi

# ── 8. summary ───────────────────────────────────────────────────────────────
echo ""
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║                        SETUP COMPLETE                       ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""
info "Useful commands:"
echo "  # Show VMI status"
echo "  ${KUBECTL_WRAPPER} --kubeconfig=${KUBECONFIG_PATH} get vmi vmi-arm"
echo ""
echo "  # Connect via VNC (requires virtctl)"
echo "  virtctl --kubeconfig=${KUBECONFIG_PATH} vnc vmi-arm"
echo ""
echo "  # Connect via serial console"
echo "  virtctl --kubeconfig=${KUBECONFIG_PATH} console vmi-arm"
echo ""
echo "  # Watch all events"
echo "  ${KUBECTL_WRAPPER} --kubeconfig=${KUBECONFIG_PATH} get events --sort-by='.lastTimestamp'"
echo ""
echo "  # Tear down cluster"
echo "  KUBEVIRT_PROVIDER=${KUBEVIRT_PROVIDER} make cluster-down"
echo ""
