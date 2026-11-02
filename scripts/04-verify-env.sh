#!/usr/bin/env bash
# 验证环境的实际效果，并确认修复扛得住 Kamaji reconcile 和 kube-proxy 重启。
# 不只看 Pod Ready。用法: 04-verify-env.sh tenant-a|tenant-b
source "$(dirname "$0")/lib.sh"
t="${1:-}"; check_tenant "$t"
node="$t-worker"
case "$t" in tenant-a) expected_gpu=10 ;; tenant-b) expected_gpu=20 ;; esac
errors=0
ok()  { echo "ok:   $*"; }
bad() { echo "FAIL: $*"; errors=$((errors + 1)); }
rules_ok() { docker exec "$node" iptables -t nat -S KUBE-SERVICES | grep -q 'default/kubernetes'; }
proxy_stable() {
  [[ "$(tk "$t" -n kube-system get pods -l k8s-app=lab-kube-proxy -o json \
    | jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | .status.containerStatuses[]? | select(.ready and .restartCount == 0)] | length')" == 1 ]]
}

log "触发 reconcile 并重启 kube-proxy"
kubectl -n tenants annotate tcp "$t" lab/verified-at="$(date +%s)" --overwrite
sleep 30
tk "$t" -n kube-system rollout restart ds/lab-kube-proxy
sleep 20  # 崩溃型故障启动后很快退出，等一会儿再判断
wait_until 90 "kube-proxy 重启后稳定" proxy_stable || bad "kube-proxy 重启后不稳定"
rules_ok && ok "ClusterIP 规则存在" || bad "ClusterIP 规则不存在"
if tk "$t" -n kube-system get ds kube-proxy >/dev/null 2>&1; then
  bad "Kamaji 又创建了 kube-proxy DaemonSet"
else
  ok "没有 Kamaji 管理的 kube-proxy"
fi

log "实际效果"
[[ "$(tk "$t" get --raw /readyz)" == ok ]] && ok "API 可访问" || bad "API 不可访问"
[[ "$(tk "$t" get node "$node" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]] \
  && ok "节点 Ready" || bad "节点不是 Ready"
gpu=$(tk "$t" get node "$node" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}')
[[ "$gpu" == "$expected_gpu" ]] && ok "nvidia.com/gpu=$gpu" || bad "nvidia.com/gpu=$gpu，期望 $expected_gpu"
# 隧道：logs 要经过 apiserver → konnectivity → kubelet。
tk "$t" -n kube-system logs ds/konnectivity-agent --tail=1 >/dev/null 2>&1 && ok "隧道可用" || bad "隧道不通"
# 调度器崩溃时 webhook 会放行，Pod 静默退回默认调度器（troubleshooting 第 4 条）。
ready=$(tk "$t" -n kube-system get deploy hami-scheduler -o jsonpath='{.status.readyReplicas}')
[[ "$ready" == 1 ]] && ok "hami-scheduler Ready" || bad "hami-scheduler readyReplicas='$ready'"

imgs=$(tk "$t" -n kube-system get pods -l app.kubernetes.io/name=hami -o json \
  | jq -r '.items[] | select(.metadata.deletionTimestamp == null) | .status.containerStatuses[]? | "\(.name) \(.image)"' | sort -u)
echo "$imgs"
if [[ -z "$imgs" ]]; then
  bad "没有找到 HAMi Pod"
elif grep -q 'projecthami/' <<<"$imgs"; then
  bad "HAMi 在用上游镜像"
else
  ok "HAMi 在用本地镜像"
fi

log "版本记录"
mkdir -p "$ROOT/results/env"
{
  echo "# $(date -Is)"
  echo "HAMi $HAMI_COMMIT"
  echo "k8s-test-infra $K8S_TEST_INFRA_COMMIT"
  echo "nvml-mock chart $NVML_MOCK_CHART_VERSION"
  echo "Kamaji chart sha256 $KAMAJI_CHART_SHA256"
  kubectl -n kamaji-system get pods -o json | jq -r '.items[].status.containerStatuses[] | .imageID' | sort -u
} | tee "$ROOT/results/env/versions.txt"

((errors == 0)) || die "$t 环境验证发现 $errors 个问题"
echo "$t 环境验证通过"
