#!/usr/bin/env bash
# HAMi webhook 本身挂掉、隧道正常时会怎样：webhook 由 hami-scheduler 提供，把它缩到 0，
# 分别在没有、有默认准入策略时跑同样的探针，再观察告警。脚本只判断实验是否有效，不预设探针结果。
# 计划要求的两项检查：有准入策略时 GPU Pod 被明确拒绝；有告警能发现 webhook 故障。
source "$(dirname "$0")/lib.sh"
EXP=webhook-down
OUT="$OUT_ROOT/$EXP"; mkdir -p "$OUT"
ALERT_WAIT="${ALERT_WAIT:-300}"
OBS_RULES=(HamiWebhookDown HamiWebhookFailOpen ApiserverToNodePathDown KonnectivityAgentDisconnected)
DENY='GPU Pod must use hami-scheduler'
exec > >(tee "$OUT/run.log") 2>&1
echo "# $EXP 开始: $(date -Is)"

guard
finish() {
  local rc=$?
  trap - EXIT
  restore_env 2>&1 | tee "$OUT/restore.txt"
  ((PIPESTATUS[0] == 0)) || { echo "环境恢复失败"; rc=1; }
  # 只等故障状态告警；HamiWebhookFailOpen 是事件型，会在最后一次放行约 5 分钟后自己解除。
  if wait_until 300 "恢复后故障状态告警全部解除" prom_none_firing "$T"; then
    echo "CHECK PASS: 恢复后告警已解除"
  else
    echo "CHECK FAIL: 恢复后仍有告警 firing"; prom_api "$T" alerts > "$OUT/alerts-after-restore.json"; rc=1
  fi
  write_summary
  echo "# $EXP 结束: $(date -Is) rc=$rc"
  sleep 1
  exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT TERM

alert_state() { prom_api "$T" alerts | jq -r --arg a "$1" '[.data.alerts[] | select(.labels.alertname == $a) | .state] | unique | join(",")'; }
scheduler_gone() { [[ -z "$(tk "$T" -n kube-system get pods -l app.kubernetes.io/component=hami-scheduler -o name 2>/dev/null)" ]]; }
# apiserver 记录的 webhook 调用结果。fail_open_count 只有发生过放行才会出现。
webhook_metrics() { tk "$T" get --raw /metrics 2>/dev/null | grep -E '^apiserver_admission_webhook_(fail_open_count|request_total|rejection_count)' | grep -F 'vgpu.hami.io' || true; }

# 上一步刚断过隧道时，隧道告警要过一会儿才解除，所以等待，而不是直接判失败。
wait_until 300 "开始前没有 firing 的故障状态告警" prom_none_firing "$T" || abort "开始前告警一直没有解除"
# 事件型的 HamiWebhookFailOpen 在上一步放行后还会持续约 5 分钟。不等它解除，就分不清这次触发是不是 webhook 故障造成的。
failopen_inactive() { [[ -z "$(alert_state HamiWebhookFailOpen)" ]]; }
wait_until 420 "开始前 HamiWebhookFailOpen 没有激活" failopen_inactive || abort "HamiWebhookFailOpen 一直没有解除"
tk "$T" get mutatingwebhookconfiguration "$WEBHOOK" -o yaml | grep -E 'failurePolicy|timeoutSeconds' | tee "$OUT/webhook-policy.txt"

# 对照组：默认部署（有准入策略），webhook 正常。
run_probes "$EXP" 对照
check_control "$EXP" || abort "对照组不成立，不做故障组"
delete_probes
webhook_metrics > "$OUT/apiserver-webhook-metrics-before.txt"

log "把 hami-scheduler 缩到 0，HAMi webhook 随之不可用"
t0=$(date +%s)
tk "$T" -n kube-system scale deploy hami-scheduler --replicas=0 || abort "缩容失败"
wait_until 120 "hami-scheduler Pod 已删除" scheduler_gone || abort "hami-scheduler Pod 没有删除"
tunnel_ok || abort "隧道不通，分不清是哪种故障"
echo "ok: 隧道正常，只有 webhook 不可用"

remove_vap
run_probes "$EXP" 故障-无准入策略
delete_probes
log "装回默认准入策略"
ensure_vap || abort "准入策略没有生效"
run_probes "$EXP" 故障-有准入策略
delete_probes
# 结果要在“只有 webhook 坏了”的条件下才有效。
tunnel_ok || abort "观察期间隧道断了，这一轮结果无效"
scheduler_gone || abort "观察期间 hami-scheduler 恢复了，这一轮结果无效"

log "观察告警，从缩容开始最多 ${ALERT_WAIT}s"
declare -A FIRED=()
while (( $(date +%s) - t0 < ALERT_WAIT )); do
  now=$(date +%s); line="$(date +%T) +$((now - t0))s"
  for a in "${OBS_RULES[@]}"; do
    s=$(alert_state "$a"); line+=" $a=${s:-inactive}"
    [[ "$s" == *firing* && -z "${FIRED[$a]:-}" ]] && FIRED[$a]=$((now - t0))
  done
  echo "$line"
  # webhook 告警已经触发，并且至少观察了 3 分钟（隧道告警是 for: 1m），就可以结束。
  [[ -n "${FIRED[HamiWebhookDown]:-}" && -n "${FIRED[HamiWebhookFailOpen]:-}" ]] && (( now - t0 >= 180 )) && break
  sleep 15
done
observed=$(( $(date +%s) - t0 ))
prom_api "$T" alerts > "$OUT/alerts-down.json"
webhook_metrics > "$OUT/apiserver-webhook-metrics-after.txt"
tunnel_ok || abort "观察期间隧道断了，这一轮结果无效"

log "结论"
{
  # 探针跑完才开始观察，所以“首次观察到”只是上限；activeAt 是 Prometheus 记录的告警开始时间（for 之前的 pending 起点）。
  for a in "${OBS_RULES[@]}"; do
    if [[ -n "${FIRED[$a]:-}" ]]; then
      active=$(jq -r --arg a "$a" '[.data.alerts[] | select(.labels.alertname == $a) | .activeAt] | min // empty' "$OUT/alerts-down.json")
      rel=""; [[ -n "$active" ]] && rel="，activeAt 在缩容后 $(( $(date -d "$active" +%s) - t0 ))s"
      echo "$a: firing，首次观察到在缩容后 ${FIRED[$a]}s${rel}"
    else
      echo "$a: 没有触发（缩容后观察到 ${observed}s）"
    fi
  done
  echo "--- apiserver 的 webhook 指标（前 → 后）"
  diff "$OUT/apiserver-webhook-metrics-before.txt" "$OUT/apiserver-webhook-metrics-after.txt" || true
} | tee "$OUT/verdict.txt"

rc=0
for p in over-card gpu-only gpu-init; do
  if [[ "$(col "$EXP" 故障-有准入策略 "$p" 6)" == 否 && "$(col "$EXP" 故障-有准入策略 "$p" 12)" == *"$DENY"* ]]; then
    echo "CHECK PASS: webhook 不可用、有准入策略时 $p 被明确拒绝"
  else
    echo "CHECK FAIL: webhook 不可用、有准入策略时 $p 没有被明确拒绝"; rc=1
  fi
done
[[ "$(col "$EXP" 故障-有准入策略 plain-probe 6)" == 是 ]] && echo "CHECK PASS: 普通 Pod 不受影响" || { echo "CHECK FAIL: 普通 Pod 没有创建"; rc=1; }
# 只有 activeAt 晚于缩容，才能说是这次 webhook 故障触发的。
for a in HamiWebhookDown HamiWebhookFailOpen; do
  active=$(jq -r --arg a "$a" '[.data.alerts[] | select(.labels.alertname == $a) | .activeAt] | min // empty' "$OUT/alerts-down.json")
  if [[ -n "${FIRED[$a]:-}" && -n "$active" ]] && (( $(date -d "$active" +%s) >= t0 )); then
    echo "CHECK PASS: $a 在缩容后触发"
  else
    echo "CHECK FAIL: $a 没有在缩容后触发（activeAt=${active:-无}）"; rc=1
  fi
done
exit "$rc"
