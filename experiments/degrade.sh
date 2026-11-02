#!/usr/bin/env bash
# 一轮降级实验：对照组 → 阻断隧道并确认 → 故障组 → 恢复。
# 用法: degrade.sh baseline|fix-a|fix-b
source "$(dirname "$0")/lib.sh"
EXP="${1:-}"
case "$EXP" in baseline|fix-a|fix-b) ;; *) die "用法: degrade.sh baseline|fix-a|fix-b" ;; esac
OUT="$OUT_ROOT/$EXP"; mkdir -p "$OUT"
exec > >(tee "$OUT/run.log") 2>&1
echo "# $EXP 开始: $(date -Is)"

guard
finish() {
  local rc=$?
  trap - EXIT
  restore_env 2>&1 | tee "$OUT/restore.txt"
  ((PIPESTATUS[0] == 0)) || { echo "环境恢复失败"; rc=1; }
  write_summary
  echo "# $EXP 结束: $(date -Is) rc=$rc"
  sleep 1
  exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT TERM

# 先保存输出再判断：lib.sh 开了 pipefail，被拒绝时 kubectl 返回 1，管道会判成失败（troubleshooting 第 8 条）。
vap_active() {
  local out; out=$(tk "$T" create --dry-run=server -f "$EXP_DIR/vap-gate-pod.yaml" 2>&1)
  [[ "$out" == *'GPU Pod must use hami-scheduler'* ]]
}
case "$EXP" in
  fix-a)
    log "方案 A：failurePolicy=Fail"
    tk "$T" patch mutatingwebhookconfiguration "$WEBHOOK" --type=json \
      -p '[{"op":"replace","path":"/webhooks/0/failurePolicy","value":"Fail"}]' || abort "patch 失败"
    [[ "$(webhook_policy)" == Fail ]] || abort "策略没有变成 Fail"
    sleep 5
    ;;
  fix-b)
    log "方案 B：ValidatingAdmissionPolicy"
    tk "$T" apply -f "$EXP_DIR/require-hami-scheduler.yaml" || abort "apply 失败"
    wait_until 90 "准入策略已生效" vap_active || abort "准入策略没有生效"
    ;;
esac
tk "$T" get mutatingwebhookconfiguration "$WEBHOOK" -o yaml | grep -E 'failurePolicy|timeoutSeconds' | tee "$OUT/webhook-policy.txt"

run_probes "$EXP" 对照
check_control "$EXP" || abort "对照组不成立，不做故障组"
delete_probes

block_tunnel
confirm_tunnel_down || abort "没有确认隧道已断，没有创建测试 Pod"
run_probes "$EXP" 故障
# 观察结束时隧道还要是断的，否则这一轮结果不可信。
check_agent_pid
tunnel_ok && abort "观察期间隧道恢复了，这一轮结果无效"
echo "ok: 观察结束时隧道仍然不通，agent PID 没有变化"

rc=0
if [[ "$EXP" == fix-b ]]; then
  # 计划对方案 B 的要求：隧道正常时能创建，断开时被明确拒绝。
  [[ "$(col fix-b 对照 over-card 6)" == 是 ]] && echo "CHECK PASS: 隧道正常时 GPU Pod 能创建" || { echo "CHECK FAIL: 隧道正常时 GPU Pod 没有创建"; rc=1; }
  for p in over-card gpu-only; do
    if [[ "$(col fix-b 故障 "$p" 6)" == 否 && "$(col fix-b 故障 "$p" 12)" == *'GPU Pod must use hami-scheduler'* ]]; then
      echo "CHECK PASS: 隧道断开时 $p 被明确拒绝"
    else
      echo "CHECK FAIL: 隧道断开时 $p 没有被明确拒绝"; rc=1
    fi
  done
fi
exit "$rc"
