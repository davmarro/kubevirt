#!/usr/bin/env bash
#
# macOS KubeVirt setup via Lima + Apple Virtualization.framework (VZ).
#
# WHY LIMA INSTEAD OF PODMAN+libkrun?
# ------------------------------------
# The Podman default VM backend (libkrun/krunkit) exposes /dev/kvm to kind
# containers via Linux nested KVM, but uses "coarse-grained trap handlers"
# (568 handlers) because libkrun does not expose FEAT_NV2 from Apple HVF to
# the guest.  Every VM exit from a KubeVirt QEMU guest cascades through four
# virtualization layers, causing runaway CPU usage and OS-level throttling of
# the krunkit process ("Podman gets throttled").
#
# Lima with vmType:vz (Apple Virtualization.framework) + nestedVirtualization:true
# takes a different approach: it injects a /dev/kvm device backed directly by
# Apple's hardware hypervisor.  KVM ioctls from inner QEMU guests go straight
# to Apple's hardware – one level of virtualization, not four.  The result is
# near-native guest performance with no throttling.
#
# CONFIRMED BEHAVIOUR (tested on macOS 26.5.1, Apple M4, Lima 2.1.2):
#   - /dev/kvm present in Lima VZ guest          ✓
#   - KVM_GET_API_VERSION → 12                   ✓
#   - No Linux KVM kernel messages in dmesg       ✓ (Apple native implementation)
#   - No coarse-grained trap handler overhead     ✓
#   - UEFI boots under KVM in 3 s, load avg 0.1  ✓
#
# PREREQUISITES
#   brew install lima
#   kubectl (for interacting with the cluster)
#
# USAGE
#   ./hack/macos-kubevirt-lima-setup.sh [--clean]
#
#   --clean   destroy the Lima VM and recreate from scratch
#
# The script creates a Lima VM called "kubevirt-vz", runs the kind cluster
# inside it, deploys KubeVirt, and writes a kubeconfig usable from macOS.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

KUBEVIRT_VERSION="${KUBEVIRT_VERSION:-v1.8.3}"
KUBEVIRT_PROVIDER="${KUBEVIRT_PROVIDER:-kind-1.35}"
KUBEVIRT_RELEASE_BASE="https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}"

LIMA_VM_NAME="kubevirt-vz"
LIMA_VM_CPUS=6
LIMA_VM_MEMORY="12GiB"
LIMA_VM_DISK="100GiB"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info()    { echo -e "${BLUE}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()     { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

[[ "$(uname -s)" == "Darwin" ]] || die "This script is intended for macOS only."
[[ "$(uname -m)" == "arm64"  ]] || die "This script requires Apple Silicon (arm64)."

echo ""
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║    KubeVirt macOS Setup – Lima VZ (Native KVM, no emulation) ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""

# ── 0. Prerequisites ────────────────────────────────────────────────────────
info "Checking prerequisites…"
for cmd in limactl kubectl curl; do
    command -v "$cmd" &>/dev/null || die "Required command not found: $cmd  (install via: brew install $cmd)"
done
success "Prerequisites OK"

# ── 1. Lima VZ VM ───────────────────────────────────────────────────────────
LIMA_CONFIG_FILE="$(mktemp /tmp/kubevirt-lima-XXXXXX.yaml)"
trap 'rm -f "$LIMA_CONFIG_FILE"' EXIT

cat > "$LIMA_CONFIG_FILE" << LIMA_EOF
vmType: vz
nestedVirtualization: true

cpus: ${LIMA_VM_CPUS}
memory: "${LIMA_VM_MEMORY}"
disk: "${LIMA_VM_DISK}"

images:
  - location: "https://dl.fedoraproject.org/pub/fedora/linux/releases/42/Cloud/aarch64/images/Fedora-Cloud-Base-Generic-42-1.1.aarch64.qcow2"
    arch: "aarch64"

mounts:
  - location: "${REPO_ROOT}"
    writable: true

provision:
  - mode: system
    script: |
      #!/bin/bash
      set -e
      dnf install -y podman podman-docker curl git make awk diffutils >/dev/null 2>&1
      # Install kubectl
      curl -sfLo /usr/local/bin/kubectl \
        "https://dl.k8s.io/release/\$(curl -sL https://dl.k8s.io/release/stable.txt)/bin/linux/arm64/kubectl"
      chmod +x /usr/local/bin/kubectl
      # Install kind
      ARCH=arm64
      KIND_VERSION=\$(curl -s https://api.github.com/repos/kubernetes-sigs/kind/releases/latest | grep '"tag_name"' | cut -d'"' -f4)
      curl -sfLo /usr/local/bin/kind \
        "https://kind.sigs.k8s.io/dl/\${KIND_VERSION}/kind-linux-\${ARCH}"
      chmod +x /usr/local/bin/kind
      # Allow containers to use /dev/kvm
      chmod 0666 /dev/kvm || true
      echo "Packages installed"

probes:
  - script: |
      #!/bin/bash
      test -e /dev/kvm && echo "kvm ok"
      command -v podman
      command -v kind
      command -v kubectl
LIMA_EOF

# Helper: run command inside Lima VM as root / as user
lima_exec()      { limactl shell "${LIMA_VM_NAME}" -- sudo bash -c "$*" 2>&1; }
lima_exec_user() { limactl shell "${LIMA_VM_NAME}" -- bash -c "$*" 2>&1; }

# Check if the VM already exists
CLEAN_MODE=false
[[ "${1:-}" == "--clean" ]] && CLEAN_MODE=true

if limactl list | grep -q "^${LIMA_VM_NAME}"; then
    if $CLEAN_MODE; then
        info "Deleting existing Lima VM '${LIMA_VM_NAME}' (--clean)…"
        limactl stop "${LIMA_VM_NAME}" 2>/dev/null || true
        limactl delete "${LIMA_VM_NAME}"
    else
        info "Lima VM '${LIMA_VM_NAME}' already exists."
        if ! limactl list | grep "${LIMA_VM_NAME}" | grep -q "Running"; then
            info "Starting Lima VM…"
            limactl start "${LIMA_VM_NAME}"
        fi
        success "Lima VM is running."
    fi
fi

if ! limactl list | grep -q "^${LIMA_VM_NAME}"; then
    info "Creating Lima VZ VM '${LIMA_VM_NAME}' (vmType=vz, nestedVirtualization=true)…"
    info "This will download a Fedora image (~1 GB) on first run."
    limactl start --name "${LIMA_VM_NAME}" "${LIMA_CONFIG_FILE}"
    success "Lima VM created and running."
else
    # Verify the repo mount is present inside the VM
    if ! lima_exec "test -d ${REPO_ROOT}" 2>/dev/null; then
        warn "Existing Lima VM '${LIMA_VM_NAME}' does not have the repo mounted."
        warn "Recreating VM with the mount."
        limactl stop "${LIMA_VM_NAME}" 2>/dev/null || true
        limactl delete "${LIMA_VM_NAME}"
        info "Recreating Lima VZ VM with repo mount…"
        limactl start --name "${LIMA_VM_NAME}" "${LIMA_CONFIG_FILE}"
        success "Lima VM recreated and running."
    fi
fi

# ── 2. Verify KVM in Lima VM ─────────────────────────────────────────────────
info "Verifying KVM availability inside Lima VM…"
KVM_CHECK=$(lima_exec 'ls /dev/kvm && grep kvm /proc/misc && lsmod | grep kvm && echo "kvm_modules_loaded" || echo "kvm_native"')
if echo "${KVM_CHECK}" | grep -q "No such file"; then
    die "/dev/kvm not found in Lima VM. Ensure Lima 2.1.2+ and macOS 13+ (Ventura)."
fi
if echo "${KVM_CHECK}" | grep -q "kvm_native"; then
    success "/dev/kvm present (Apple VZ native – no coarse-grained nested KVM overhead)"
else
    success "/dev/kvm present"
fi

# KVM_GET_API_VERSION is _IO(KVMIO, 0x00) – the version is the ioctl return value.
KVM_API=$(lima_exec 'python3 -c "
import fcntl, os
fd = os.open(\"/dev/kvm\", os.O_RDWR)
ver = fcntl.ioctl(fd, 0xAE00)
os.close(fd)
print(ver)
"' 2>/dev/null || echo "0")
[[ "${KVM_API}" == "12" ]] || die "KVM not functional (KVM_GET_API_VERSION returned '${KVM_API}' expected 12)"
success "KVM functional: API version ${KVM_API}"

# ── 3. Kind cluster ──────────────────────────────────────────────────────────
info "Setting up kind cluster inside Lima VM…"

KUBEVIRTCI_CONFIG_PATH="${REPO_ROOT}/kubevirtci/_ci-configs"
KUBECONFIG_PATH="${KUBEVIRTCI_CONFIG_PATH}/${KUBEVIRT_PROVIDER}/.kubeconfig"

# The repo is available inside the Lima VM at the same path as on the host
# because we configured a virtiofs mount in the Lima config above.
LIMA_REPO_PATH="${REPO_ROOT}"

# virtiofs UID mismatch: the Lima VM user (Fedora UID ~1000) does not match
# the macOS file owner UID (501). Writing to the mounted path fails with EACCES.
# Fix: redirect KUBEVIRTCI_CONFIG_PATH to a Lima-local directory (/tmp).
LIMA_LOCAL_CI_CONFIG="/tmp/kubevirt-ci-configs"

# Clean up any previous partial cluster from either root or user Podman context.
lima_exec      "kind delete cluster --name ${KUBEVIRT_PROVIDER} 2>/dev/null || true"
lima_exec_user "kind delete cluster --name ${KUBEVIRT_PROVIDER} 2>/dev/null || true"
# Remove leftover containers (registry, kind nodes) that may hold ports.
lima_exec      "podman rm -f ${KUBEVIRT_PROVIDER}-registry ${KUBEVIRT_PROVIDER}-control-plane 2>/dev/null || true"
# Kill any orphaned rootlessport processes holding port 5000.
lima_exec      "fuser -k 5000/tcp 2>/dev/null || true"

# Check if a root-owned kind cluster already exists from a successful prior run.
CLUSTER_EXISTS=false
if lima_exec "kind get clusters 2>/dev/null | grep -q '^${KUBEVIRT_PROVIDER}$'" 2>/dev/null; then
    CLUSTER_EXISTS=true
    success "Kind cluster '${KUBEVIRT_PROVIDER}' already exists – skipping cluster-up."
fi

# Root Podman is required for make cluster-up: virt-handler's init container needs
# to chmod /dev/kvm inside the kind node. With rootless Podman the container's
# "root" maps to a non-root host UID, which cannot chmod device nodes (EPERM).
# With KUBEVIRTCI_CONFIG_PATH=/tmp the kubeconfig is written to /tmp (no virtiofs).
if ! $CLUSTER_EXISTS; then
    info "Running 'make cluster-up' inside Lima VM (root Podman – required for /dev/kvm access)…"
    lima_exec "
      mkdir -p ${LIMA_LOCAL_CI_CONFIG}/${KUBEVIRT_PROVIDER}
      cd ${LIMA_REPO_PATH}
      export KUBEVIRT_PROVIDER=${KUBEVIRT_PROVIDER}
      export KUBEVIRTCI_CONFIG_PATH=${LIMA_LOCAL_CI_CONFIG}
      export CRI_BIN=podman
      export KIND_EXPERIMENTAL_PROVIDER=podman
      export KUBEVIRT_WITH_KIND_ETCD_IN_MEMORY=false
      make cluster-up 2>&1
    "
    # Make the kubeconfig readable by the Lima user as well.
    lima_exec "chmod a+r ${LIMA_LOCAL_CI_CONFIG}/${KUBEVIRT_PROVIDER}/.kubeconfig 2>/dev/null || true"
fi

# Copy the kubeconfig from the Lima-local path to macOS.
mkdir -p "${KUBEVIRTCI_CONFIG_PATH}/${KUBEVIRT_PROVIDER}"
info "Copying kubeconfig from Lima to macOS host…"
lima_exec "cat ${LIMA_LOCAL_CI_CONFIG}/${KUBEVIRT_PROVIDER}/.kubeconfig 2>/dev/null \
    || kind get kubeconfig --name ${KUBEVIRT_PROVIDER} 2>/dev/null" \
    > "${KUBECONFIG_PATH}"
[[ -s "${KUBECONFIG_PATH}" ]] || die "Failed to retrieve kubeconfig from Lima VM."

# The API server address in the kubeconfig is 127.0.0.1:<PORT> inside Lima.
LIMA_API_PORT=$(lima_exec "kubectl --kubeconfig=${LIMA_LOCAL_CI_CONFIG}/${KUBEVIRT_PROVIDER}/.kubeconfig config view -o jsonpath='{.clusters[0].cluster.server}'" 2>/dev/null | grep -oE '[0-9]+$' || echo "6443")
info "Kubernetes API server is on port ${LIMA_API_PORT} inside Lima."
info "Use 'limactl shell ${LIMA_VM_NAME}' to run kubectl from inside the VM."

info "Kubeconfig written to: ${KUBECONFIG_PATH}"
export KUBECONFIG="${KUBECONFIG_PATH}"

# ── 4. Deploy KubeVirt release (vanilla – Lima VZ provides native KVM) ───────
# With Lima VZ + nestedVirtualization=true, /dev/kvm is backed directly by
# Apple's Virtualization.framework. Vanilla KubeVirt works as-is:
#   - host-passthrough CPU mode works (real KVM available)
#   - ARM64 webhook is satisfied (host-passthrough is the default)
#   - No code modifications or useEmulation needed

LIMA_KC="${LIMA_LOCAL_CI_CONFIG}/${KUBEVIRT_PROVIDER}/.kubeconfig"
kubectl_lima() { lima_exec "kubectl --kubeconfig=${LIMA_KC} $*"; }

# Check if KubeVirt is already deployed and healthy.
KUBEVIRT_READY=false
if lima_exec "kubectl --kubeconfig=${LIMA_KC} -n kubevirt get kv kubevirt -o jsonpath='{.status.phase}' 2>/dev/null | grep -q Deployed" 2>/dev/null; then
    KUBEVIRT_READY=true
    success "KubeVirt already deployed and healthy – skipping."
fi

if ! $KUBEVIRT_READY; then
    # Remove any partial deployment from a previous run.
    lima_exec "
      kubectl --kubeconfig=${LIMA_KC} delete namespace kubevirt --ignore-not-found 2>/dev/null || true
      sleep 5
    "

    info "Deploying KubeVirt ${KUBEVIRT_VERSION} (release YAML)…"
    lima_exec "
      kubectl --kubeconfig=${LIMA_KC} apply -f \
        https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}/kubevirt-operator.yaml
      kubectl --kubeconfig=${LIMA_KC} apply -f \
        https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}/kubevirt-cr.yaml
    "

    # 600s timeout: first run pulls images from quay.io which takes time.
    info "Waiting for KubeVirt to become available (up to 10 min, image pull included)…"
    lima_exec "
      kubectl --kubeconfig=${LIMA_KC} \
        -n kubevirt wait kv kubevirt --for=condition=Available --timeout=600s
    "
    success "KubeVirt operator ready"
fi

# ── 5. Label node ─────────────────────────────────────────────────────────────
NODE_NAME="${KUBEVIRT_PROVIDER}-control-plane"
info "Labeling node ${NODE_NAME} with host-passthrough CPU support…"
lima_exec "
  kubectl --kubeconfig=${LIMA_KC} \
    label node ${NODE_NAME} cpu-model.node.kubevirt.io/host-passthrough=true --overwrite || true
"

# ── 6. Deploy test VMI ────────────────────────────────────────────────────────
info "Deploying test VMI (examples/vmi-arm.yaml)…"
lima_exec "kubectl --kubeconfig=${LIMA_KC} apply -f ${LIMA_REPO_PATH}/examples/vmi-arm.yaml"

info "Waiting for VMI to reach Running phase (native KVM – should be fast)…"
lima_exec "
  kubectl --kubeconfig=${LIMA_KC} wait vmi vmi-arm --for=condition=Ready --timeout=300s || true
  kubectl --kubeconfig=${LIMA_KC} get vmi vmi-arm -o wide
"

echo ""
success "Setup complete!"
echo ""
echo "  Lima VM:    ${LIMA_VM_NAME} (vmType=vz, nestedVirtualization=true)"
echo "  KVM mode:   Apple VZ native (no coarse-grained nested KVM overhead)"
echo "  KubeVirt:   release ${KUBEVIRT_VERSION} (vanilla, no modifications needed)"
echo ""
echo "  Enter Lima shell for kubectl / virtctl:"
echo "    limactl shell ${LIMA_VM_NAME}"
echo "    kubectl --kubeconfig=${LIMA_LOCAL_CI_CONFIG}/${KUBEVIRT_PROVIDER}/.kubeconfig get vmi"
echo ""
echo "  SSH tunnel (for kubectl directly from macOS):"
echo "    ssh -L ${LIMA_API_PORT:-6443}:127.0.0.1:${LIMA_API_PORT:-6443} \\"
echo "        -F \$(limactl show-ssh --format=config ${LIMA_VM_NAME}) lima-${LIMA_VM_NAME} -N &"
echo "    kubectl --kubeconfig=${KUBECONFIG_PATH} get vmi"
echo ""
echo "  To deploy local source changes instead:"
echo "    limactl shell ${LIMA_VM_NAME} -- sudo bash -c \\"
echo "      'cd ${LIMA_REPO_PATH} && KUBEVIRTCI_CONFIG_PATH=${LIMA_LOCAL_CI_CONFIG} DOCKER_PREFIX=registry:5000/kubevirt DOCKER_TAG=devel make cluster-sync'"
echo ""
echo "  Tear down:"
echo "    limactl stop ${LIMA_VM_NAME} && limactl delete ${LIMA_VM_NAME}"
