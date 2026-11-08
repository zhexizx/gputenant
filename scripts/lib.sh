#!/usr/bin/env bash
# 环境脚本和测试脚本共用的路径、版本和小工具。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TOOLS_BIN="$ROOT/.tools/bin"
CACHE_DIR="$ROOT/.cache"
KUBECONFIG_DIR="$ROOT/kubeconfigs"
MGMT_KUBECONFIG="$KUBECONFIG_DIR/management.kubeconfig"
# 租户 kubeconfig 的根目录，gputenant CLI 用同一个变量。脚本默认放在 kubeconfigs/ 下。
export GPUTENANT_HOME="${GPUTENANT_HOME:-$KUBECONFIG_DIR}"
GPUTENANT_BIN="$ROOT/bin/gputenant"
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
PROMETHEUS_CHART_REPO=https://prometheus-community.github.io/helm-charts
PROMETHEUS_CHART_VERSION=29.40.3
GUARDRAILS_DIR="$ROOT/manifests/guardrails"
# 默认部署的告警规则。前三条反映持续的故障状态；HamiWebhookFailOpen 是事件型，放行后约 5 分钟才解除。
GUARDRAIL_STATE_ALERTS=(KonnectivityAgentDisconnected ApiserverToNodePathDown HamiWebhookDown)
GUARDRAIL_ALERTS=("${GUARDRAIL_STATE_ALERTS[@]}" HamiWebhookFailOpen)

# 始终用项目自己的 kubeconfig，不改动 ~/.kube/config 的当前 context。
export KUBECONFIG="$MGMT_KUBECONFIG"

log() { printf '\n[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

tenant_kubeconfig() { printf '%s/%s/kubeconfig' "$GPUTENANT_HOME" "$1"; }
tenant_internal_kubeconfig() { printf '%s/%s/internal.kubeconfig' "$GPUTENANT_HOME" "$1"; }
# 代理端口由 CLI 分配（从 16443 起第一个空闲的），从代理容器读取。
tenant_api_port() { docker port "$1-api-proxy" 6443/tcp | head -1 | sed 's/.*://'; }
# 租户的 GPU 配置，和 README 的 TC 设计一致。
tenant_gpu_args() { case "$1" in tenant-a) echo "--gpu-profile a100 --gpu-count 1" ;; tenant-b) echo "--gpu-profile t4 --gpu-count 2" ;; esac; }
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

# ---- 默认防护（05-tenant-guardrails.sh 部署）的检查 ----

# vap_denies <租户>：准入策略已生效。门禁 Pod 带 hami.io/webhook=ignore 标签，会跳过 webhook，必须被拒绝。
# 先存下输出再 grep：被拒绝时 kubectl 返回 1，pipefail 下管道会判为失败（troubleshooting 第 8 条）。
vap_denies() {
  local out
  out=$(tk "$1" create --dry-run=server -f "$GUARDRAILS_DIR/vap-gate-pod.yaml" 2>&1)
  grep -q 'GPU Pod must use hami-scheduler' <<<"$out"
}
# prom_api <租户> <API 路径> [curl 参数...]：在节点内经 ClusterIP 查询 Prometheus。
# 不用 port-forward，因为它也经过隧道。
prom_api() {
  local t="$1" path="$2" ip; shift 2
  ip=$(tk "$t" -n monitoring get svc prometheus-server -o jsonpath='{.spec.clusterIP}') && [[ -n "$ip" ]] || return 1
  docker exec "$t-worker" curl -sS -m 10 -G "http://$ip/api/v1/$path" "$@"
}
# prom_query <租户> <PromQL>：返回所有结果值，用逗号分隔。
prom_query() {
  local out
  out=$(prom_api "$1" query --data-urlencode "query=$2") && jq -r '[.data.result[].value[1]] | join(",")' <<<"$out"
}
# 默认的告警规则都已加载。
prom_rules_loaded() {
  local names a
  names=$(prom_api "$1" rules | jq -r '.data.groups[].rules[].name') || return 1
  for a in "${GUARDRAIL_ALERTS[@]}"; do grep -qx "$a" <<<"$names" || return 1; done
}
# 告警依赖的三个信号都是正常值。
prom_signals_ok() {
  [[ "$(prom_query "$1" 'up{job="tunnel-probe"}')" == 1 ]] &&
    [[ "$(prom_query "$1" 'up{job="hami-scheduler"}')" == 1 ]] &&
    [[ "$(prom_query "$1" konnectivity_network_proxy_agent_open_server_connections)" == 1 ]]
}
# 没有处于 firing 的故障状态告警。不看事件型的 HamiWebhookFailOpen：故障恢复后它还会持续约 5 分钟。
prom_none_firing() {
  local out names
  names=$(printf '%s\n' "${GUARDRAIL_STATE_ALERTS[@]}" | jq -R . | jq -sc .)
  out=$(prom_api "$1" alerts) &&
    [[ "$(jq --argjson n "$names" '[.data.alerts[] | select(.state == "firing" and (.labels.alertname as $a | $n | index($a)))] | length' <<<"$out")" == 0 ]]
}
