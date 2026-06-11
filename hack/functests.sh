#!/usr/bin/env bash
#
# This file is part of the KubeVirt project
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Copyright 2017 Red Hat, Inc.
#

set -e

DOCKER_TAG=${DOCKER_TAG:-devel}
DOCKER_TAG_ALT=${DOCKER_TAG_ALT:-devel_alt}
KUBEVIRT_E2E_PARALLEL_NODES=${KUBEVIRT_E2E_PARALLEL_NODES:-4}
KUBEVIRT_FUNC_TEST_GINKGO_ARGS=${FUNC_TEST_ARGS:-${KUBEVIRT_FUNC_TEST_GINKGO_ARGS}}
KUBEVIRT_FUNC_TEST_LABEL_FILTER=${FUNC_TEST_LABEL_FILTER:-${KUBEVIRT_FUNC_TEST_LABEL_FILTER}}
KUBEVIRT_FUNC_TEST_GINKGO_TIMEOUT=${KUBEVIRT_FUNC_TEST_GINKGO_TIMEOUT:-4h}

source hack/common.sh
source hack/config.sh

_default_previous_release_registry="quay.io/kubevirt"

previous_release_registry=${PREVIOUS_RELEASE_REGISTRY:-$_default_previous_release_registry}

functest_docker_prefix=${manifest_docker_prefix-${docker_prefix}}

echo "Using $kubevirt_test_config as test configuration"

virtctl_path=$(pwd)/_out/cmd/virtctl/virtctl
example_guest_agent_path=$(pwd)/_out/cmd/example-guest-agent/example-guest-agent

rm -rf $ARTIFACTS
mkdir -p $ARTIFACTS

function functest() {
    KUBEVIRT_FUNC_TEST_SUITE_ARGS="--ginkgo.trace
	    -apply-default-e2e-configuration \
	    -conn-check-ipv4-address=${conn_check_ipv4_address} \
	    -conn-check-ipv6-address=${conn_check_ipv6_address} \
	    -conn-check-dns=${conn_check_dns} \
	    -migration-network-nic=${migration_network_nic} \
	    ${KUBEVIRT_FUNC_TEST_SUITE_ARGS}"
    if [[ ${KUBEVIRT_PROVIDER} =~ .*(k8s-sriov).* ]] || [[ ${KUBEVIRT_SINGLE_STACK} == "true" ]]; then
        echo "Will skip test asserting the cluster is in dual-stack mode."
        KUBEVIRT_FUNC_TEST_SUITE_ARGS="-skip-dual-stack-test ${KUBEVIRT_FUNC_TEST_SUITE_ARGS}"
    fi

    local kubectl_for_tests=${kubectl}
    if [[ "$(uname -s)" == "Darwin" ]]; then
        local linux_kubectl="$(pwd)/_out/tools/kubectl-linux"
        if [[ ! -x "${linux_kubectl}" ]]; then
            echo "Downloading Linux kubectl for container execution..."
            mkdir -p "$(pwd)/_out/tools"
            local arch
            arch=$(uname -m)
            [[ "${arch}" == "arm64" ]] && arch="arm64" || arch="amd64"
            curl -sL "https://dl.k8s.io/release/$(curl -sL https://dl.k8s.io/release/stable.txt)/bin/linux/${arch}/kubectl" \
                -o "${linux_kubectl}" && chmod +x "${linux_kubectl}"
        fi
        kubectl_for_tests="${linux_kubectl}"
    fi

    local ginkgo_cmd=(
        _out/tests/ginkgo
        -timeout=${KUBEVIRT_FUNC_TEST_GINKGO_TIMEOUT}
        -r "$@"
        _out/tests/tests.test
        --
        -kubeconfig=${kubeconfig}
        -container-tag=${docker_tag}
        -container-tag-alt=${docker_tag_alt}
        -container-prefix=${functest_docker_prefix}
        -image-prefix-alt=${image_prefix_alt}
        -kubectl-path=${kubectl_for_tests}
        -installed-namespace=${namespace}
        -previous-release-tag=${PREVIOUS_RELEASE_TAG}
        -previous-release-registry=${previous_release_registry}
        -deploy-testing-infra=${deploy_testing_infra}
        -config=${kubevirt_test_config}
        --artifacts=${ARTIFACTS}
        --operator-manifest-path=${OPERATOR_MANIFEST_PATH}
        --testing-manifest-path=${TESTING_MANIFEST_PATH}
        ${KUBEVIRT_FUNC_TEST_SUITE_ARGS}
        -virtctl-path=${virtctl_path}
        -example-guest-agent-path=${example_guest_agent_path}
    )

    if [[ "$(uname -s)" == "Darwin" ]]; then
        if [[ -z "${kubeconfig}" ]]; then
            echo "ERROR: kubeconfig is not set. Run 'export KUBECONFIG=/path/to/kubeconfig' and 'make cluster-up' first." >&2
            exit 1
        fi

        echo "macOS detected: running functest binaries inside a Linux container via ${KUBEVIRT_CRI}"
        local resolved_kubeconfig
        resolved_kubeconfig=$(cd "$(dirname "${kubeconfig}")" && pwd)/$(basename "${kubeconfig}")

        local volumes=(
            -v "$(pwd):$(pwd)"
        )
        case "${resolved_kubeconfig}" in
        "$(pwd)"*) ;;
        *) volumes+=(-v "${resolved_kubeconfig}:${resolved_kubeconfig}:ro") ;;
        esac

        ${KUBEVIRT_CRI} run --rm \
            "${volumes[@]}" \
            -w "$(pwd)" \
            fedora:latest \
            "${ginkgo_cmd[@]}"
    else
        "${ginkgo_cmd[@]}"
    fi
}

additional_test_args=()
if [ -n "$KUBEVIRT_E2E_SKIP" ]; then
    additional_test_args+=("--skip=${KUBEVIRT_E2E_SKIP}")
fi

if [ -n "$KUBEVIRT_E2E_FOCUS" ]; then
    additional_test_args+=("--focus=${KUBEVIRT_E2E_FOCUS}")
fi

if [ "$KUBEVIRT_E2E_PARALLEL" == "true" ]; then
    additional_test_args+=("--nodes=${KUBEVIRT_E2E_PARALLEL_NODES}")
fi

set -x
if [ -n "${KUBEVIRT_FUNC_TEST_LABEL_FILTER}" ]; then
    additional_test_args+=("--label-filter=${KUBEVIRT_FUNC_TEST_LABEL_FILTER}")
fi
functest "${additional_test_args[@]}" ${KUBEVIRT_FUNC_TEST_GINKGO_ARGS}
