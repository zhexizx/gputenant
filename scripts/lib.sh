#!/usr/bin/env bash
# 环境脚本和测试脚本共用的路径、版本和小工具。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TOOLS_BIN="$ROOT/.tools/bin"
CACHE_DIR="$ROOT/.cache"
KUBECONFIG_DIR="$ROOT/kubeconfigs"
MGMT_KUBECONFIG="$KUBECONFIG_DIR/management.kubeconfig"
export PATH="$TOOLS_BIN:$PATH"

MGMT_CLUSTER=kamaji
HELM_VERSION=v3.19.0
JQ_VERSION=1.8.2
CERT_MANAGER_VERSION=v1.21.2
METALLB_VERSION=v0.16.1
CALICO_VERSION=v3.31.2
# 上游的 0.0.0+latest 和 latest 会随推送变化，所以把 chart 存进仓库，镜像固定 digest。
KAMAJI_CHART="$ROOT/manifests/charts/kamaji-0.0.0+latest.tgz"
KAMAJI_CHART_SHA256=983d19e2d11b11dbffe3271ad0e49d09d2d781791875bbe5ff5d7eef25063545
KAMAJI_IMAGE_TAG=latest@sha256:9f0cae2a6d134727853f49676fb0a9888f896b8aa3537253aeaa1bdee0029759
K8S_VERSION=v1.36.4
NODE_IMAGE="kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed"
POD_CIDR=10.244.0.0/16
PREFLIGHT_IMAGE=alpine:3.22
HAMI_COMMIT=2b8f4e29c48bca376d4f6123d7c6ae7627aef579
K8S_TEST_INFRA_COMMIT=e7f2320c95bef01ddde12fb8c493019a558e8f43
SRC_DIR="$HOME/src"
NVML_MOCK_CHART_VERSION=0.6.0
SOCAT_IMAGE=alpine/socat:1.8.0.3

# 始终用项目自己的 kubeconfig，不改动 ~/.kube/config 的当前 context。
export KUBECONFIG="$MGMT_KUBECONFIG"

log() { printf '\n[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

tenant_kubeconfig() { printf '%s/%s.kubeconfig' "$KUBECONFIG_DIR" "$1"; }
tenant_internal_kubeconfig() { printf '%s/%s.internal.kubeconfig' "$KUBECONFIG_DIR" "$1"; }
tenant_api_port() { case "$1" in tenant-a) echo 16443 ;; tenant-b) echo 16444 ;; esac; }
tk() { local t="$1"; shift; kubectl --kubeconfig "$(tenant_kubeconfig "$t")" "$@"; }

check_tenant() {
  case "${1:-}" in
    tenant-a|tenant-b) ;;
    *) die "tenant 必须是 tenant-a 或 tenant-b，收到: '${1:-}'" ;;
  esac
}

# wait_until <超时秒> <描述> <命令...>：轮询到命令成功，超时返回 1。
wait_until() {
  local timeout="$1" desc="$2"; shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    if ((SECONDS >= deadline)); then
      printf 'TIMEOUT(%ss): %s\n' "$timeout" "$desc" >&2
      return 1
    fi
    sleep 5
  done
  printf 'ok: %s\n' "$desc"
}
