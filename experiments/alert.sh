#!/usr/bin/env bash
# 安装 Prometheus，断开隧道，用 Prometheus API 记录告警。没有告警进入 firing 就算失败。
# 查询在节点内经 ClusterIP 进行，不用 port-forward，因为它也依赖隧道。
source "$(dirname "$0")/lib.sh"
CHART_VERSION=29.40.3
ALERT_WAIT="${ALERT_WAIT:-300}"
RULES=(KonnectivityAgentUnavailable KonnectivityAgentDisconnected ApiserverToNodePathDown)
OUT="$OUT_ROOT/alert"; mkdir -p "$OUT"
exec > >(tee "$OUT/run.log") 2>&1
echo "# alert 开始: $(date -Is)"
guard

log "Prometheus chart $CHART_VERSION"
helm upgrade --install prometheus prometheus --repo https://prometheus-community.github.io/helm-charts \
  --version "$CHART_VERSION" -n monitoring --create-namespace --kubeconfig "$(tenant_kubeconfig "$T")" \
  -f "$EXP_DIR/prom-values.yaml" --wait --timeout 8m > "$OUT/helm.txt" 2>&1 || { cat "$OUT/helm.txt"; abort "Prometheus 安装失败"; }
tk "$T" apply -f "$EXP_DIR/prom-tunnel-probe-rbac.yaml" || abort "RBAC apply 失败"
tk "$T" -n monitoring get pods -o wide | tee "$OUT/monitoring-pods.txt"
PROM=$(tk "$T" -n monitoring get svc prometheus-server -o jsonpath='{.spec.clusterIP}')
[[ -n "$PROM" ]] || abort "读不到 prometheus-server 的 ClusterIP"

prom() { docker exec "$NODE" curl -sS -m 10 -G "http://$PROM/api/v1/$1" "${@:2}"; }
query() { prom query --data-urlencode "query=$1" | jq -r '[.data.result[].value[1]] | join(",")'; }
alert_state() { prom alerts | jq -r --arg a "$1" '[.data.alerts[] | select(.labels.alertname == $a) | .state] | unique | join(",")'; }
metrics_line() {
  echo "gauge=[$(query konnectivity_network_proxy_agent_open_server_connections)] up_probe=[$(query 'up{job="tunnel-probe"}')] ds_unavailable=[$(query 'kube_daemonset_status_number_unavailable{daemonset="konnectivity-agent"}')]"
}
signals_ready() {
  [[ "$(query konnectivity_network_proxy_agent_open_server_connections)" == 1 ]] \
    && [[ "$(query 'up{job="tunnel-probe"}')" == 1 ]] \
    && [[ "$(query 'kube_daemonset_status_number_unavailable{daemonset="konnectivity-agent"}')" == 0 ]]
}
rules_loaded() {
  local a
  for a in "${RULES[@]}"; do
    prom rules | jq -e --arg a "$a" '[.data.groups[].rules[].name] | index($a)' >/dev/null || return 1
  done
}
none_active() { [[ "$(prom alerts | jq -r '[.data.alerts[] | select(.labels.alertname as $n | ["KonnectivityAgentUnavailable","KonnectivityAgentDisconnected","ApiserverToNodePathDown"] | index($n))] | length')" == 0 ]]; }

wait_until 120 "规则已加载" rules_loaded || abort "规则没有加载"
wait_until 240 "三个信号都有正常值" signals_ready || { metrics_line; abort "隧道正常时信号不正常"; }
wait_until 180 "隧道正常时没有告警" none_active || { prom alerts | tee "$OUT/alerts-baseline.json"; abort "隧道正常时就有告警"; }
metrics_line | tee "$OUT/baseline.txt"
prom alerts > "$OUT/alerts-baseline.json"

finish() {
  local rc=$?
  trap - EXIT
  restore_env 2>&1 | tee "$OUT/restore.txt"
  ((PIPESTATUS[0] == 0)) || { echo "环境恢复失败"; rc=1; }
  echo "# alert 结束: $(date -Is) rc=$rc"
  sleep 1
  exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT TERM

start=$(date +%s)
block_tunnel
confirm_tunnel_down || abort "没有确认隧道已断"

log "隧道已断，观察 ${ALERT_WAIT}s"
declare -A FIRED=()
deadline=$((SECONDS + ALERT_WAIT)); n=0
while ((SECONDS < deadline)); do
  check_agent_pid
  n=$((n + 1)); line="$(date +%T)"
  for a in "${RULES[@]}"; do
    s=$(alert_state "$a"); line+=" $a=${s:-inactive}"
    [[ "$s" == *firing* && -z "${FIRED[$a]:-}" ]] && FIRED[$a]=$(($(date +%s) - start))
  done
  echo "$line $(metrics_line)"
  ((n % 4 == 1)) && prom alerts > "$OUT/alerts-down-$n.json"
  sleep 15
done
prom alerts > "$OUT/alerts-down-final.json"
check_agent_pid
tunnel_ok && abort "观察期间隧道恢复了，这一轮结果无效"
down_end=$(date +%s)

log "恢复隧道，确认告警解除"
unblock_tunnel
wait_until 300 "隧道恢复" tunnel_ok || abort "隧道没有恢复"
resolved=0
wait_until 300 "告警全部解除" none_active && resolved=1
metrics_line | tee "$OUT/after-restore.txt"
prom alerts > "$OUT/alerts-after-restore.json"
prom query_range --data-urlencode 'query=ALERTS{alertname=~"KonnectivityAgentUnavailable|KonnectivityAgentDisconnected|ApiserverToNodePathDown"}' \
  --data-urlencode "start=$start" --data-urlencode "end=$(date +%s)" --data-urlencode step=15 > "$OUT/alerts-history.json"

log "结论"
rc=0; fired=0
{
  for a in "${RULES[@]}"; do
    if [[ -n "${FIRED[$a]:-}" ]]; then
      echo "$a: firing，阻断后 ${FIRED[$a]}s"; fired=1
    else
      echo "$a: 没有触发（隧道断开观察到 $((down_end - start))s）"
    fi
  done
  ((resolved)) && echo "恢复后告警已解除" || echo "恢复后告警没有解除"
} | tee "$OUT/verdict.txt"
for a in "${RULES[@]}"; do [[ -n "${FIRED[$a]:-}" ]] && fired=1; done
((fired)) && echo "CHECK PASS: 有告警真实触发" || { echo "CHECK FAIL: 没有告警触发"; rc=1; }
((resolved)) && echo "CHECK PASS: 告警已解除" || { echo "CHECK FAIL: 告警没有解除"; rc=1; }
exit "$rc"
