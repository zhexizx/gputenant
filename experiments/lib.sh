#!/usr/bin/env bash
# Day 4-5 实验共用：前置检查、阻断和恢复隧道、探针记录。
# 脚本只判断实验是否有效（对照组、隧道状态、环境恢复），不预设故障组的结果。
source "$(dirname "${BASH_SOURCE[0]}")/../scripts/lib.sh"
set +e

T=tenant-b
NODE=tenant-b-worker
EXP_DIR="$ROOT/manifests/experiments"
WEBHOOK=hami-webhook
ORIG_POLICY=Ignore
PROBES=(over-card gpu-only plain-probe)
OBSERVE="${OBSERVE:-60}"
OUT_ROOT="${OUT_ROOT:-$ROOT/results/day4-5-$(date +%Y%m%d-%H%M%S)}"; export OUT_ROOT
LB_IP=$(kubectl -n tenants get svc "$T" -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
[[ -n "$LB_IP" ]] || die "读不到 $T 的 LB IP"
FW=(-d "$LB_IP" -p tcp --dport 8132 -j DROP)

abort() { printf '\nABORT: %s\n' "$*"; exit 1; }
webhook_policy() { tk "$T" get mutatingwebhookconfiguration "$WEBHOOK" -o jsonpath='{.webhooks[0].failurePolicy}'; }
vap_exists() { tk "$T" get validatingadmissionpolicy require-hami-scheduler >/dev/null 2>&1; }
api_ok() { [[ "$(tk "$T" --request-timeout=10s get --raw /readyz 2>/dev/null)" == ok ]]; }
# logs 要走 apiserver → konnectivity → kubelet，和 webhook 调用一样经过隧道。
tunnel_ok() { tk "$T" --request-timeout=20s -n kube-system logs ds/konnectivity-agent --tail=1 >/dev/null 2>&1; }
# 阻断规则加在 agent Pod 的网络命名空间里。节点的 FORWARD 链不行：Felix 会把 cali-FORWARD 插回最前面，
# cali-fw-* 对已有连接直接 ACCEPT，DROP 只拦得住新连接（见 troubleshooting 第 7 条）。
# 规则属于 Pod 沙箱的命名空间：容器重启后还在，Pod 重建后就没了。
agent_pid() {
  local id
  id=$(docker exec "$NODE" crictl ps -q --name '^konnectivity-agent$' --state running 2>/dev/null | head -1)
  [[ -n "$id" ]] && docker exec "$NODE" crictl inspect -o go-template --template '{{.info.pid}}' "$id" 2>/dev/null
}
in_agent_ns() {
  local pid; pid=$(agent_pid)
  [[ -n "$pid" ]] || { echo "找不到运行中的 konnectivity-agent 容器" >&2; return 1; }
  docker exec "$NODE" nsenter -t "$pid" -n "$@"
}
fw_present() { in_agent_ns iptables -C OUTPUT "${FW[@]}" 2>/dev/null; }
# 旧版本加在节点上的规则，只用于检查和清理。
node_fw_present() { docker exec "$NODE" iptables -C "$1" "${FW[@]}" 2>/dev/null; }

# 确认没有其他测试在运行，并且环境是干净的。
guard() {
  log "前置检查"
  if [[ -z "${LAB_LOCK_HELD:-}" ]]; then
    mkdir -p "$CACHE_DIR"
    exec 9>"$CACHE_DIR/experiment.lock"
    flock -n 9 || abort "另一个实验正在运行"
    export LAB_LOCK_HELD=1
  fi
  local others c pods
  others=$(pgrep -af 'tests/(run-all|tc-[0-9]+)\.sh|scripts/(up|down|verify-from-scratch|0[0-9]-[a-z-]+)\.sh' || true)
  [[ -z "$others" ]] || abort "有其他测试或环境脚本在运行：$others"
  echo "ok: 没有其他测试或环境脚本在运行"
  fw_present && abort "agent 命名空间里有上次留下的阻断规则，先运行 experiments/restore.sh"
  for c in OUTPUT FORWARD; do node_fw_present "$c" && abort "节点 $c 链有旧版本留下的阻断规则，先运行 experiments/restore.sh"; done
  [[ "$(webhook_policy)" == "$ORIG_POLICY" ]] || abort "webhook 策略不是 $ORIG_POLICY，先运行 experiments/restore.sh"
  vap_exists && abort "准入策略还在，先运行 experiments/restore.sh"
  pods=$(tk "$T" get pods -n default -o name | grep -v '^pod/nvml-mock-' || true)
  [[ -z "$pods" ]] || abort "default 命名空间有其他 Pod：$pods"
  api_ok || abort "租户 API 不可用"
  tunnel_ok || abort "实验开始前隧道就不通"
  echo "ok: 环境干净，API 和隧道正常，LB=$LB_IP"
}

block_tunnel() {
  AGENT_PID=$(agent_pid)
  [[ -n "$AGENT_PID" ]] || abort "找不到运行中的 konnectivity-agent 容器"
  log "阻断隧道：agent（PID $AGENT_PID）的命名空间 → $LB_IP:8132"
  fw_present || in_agent_ns iptables -I OUTPUT "${FW[@]}" || abort "加阻断规则失败"
  in_agent_ns iptables -S OUTPUT
  in_agent_ns ss -tn 2>/dev/null | grep -F ":8132" || true
}
# agent 换了 PID，说明容器重启或 Pod 重建，阻断可能已经失效，这一轮结果不可信。
check_agent_pid() {
  local now; now=$(agent_pid)
  [[ "$now" == "$AGENT_PID" ]] || abort "阻断期间 agent PID 从 $AGENT_PID 变成 '${now}'，这一轮结果无效"
  fw_present || abort "阻断规则不见了，这一轮结果无效"
}
unblock_tunnel() {
  local c
  while fw_present; do in_agent_ns iptables -D OUTPUT "${FW[@]}"; done
  for c in OUTPUT FORWARD; do
    while node_fw_present "$c"; do docker exec "$NODE" iptables -D "$c" "${FW[@]}"; done
  done
}
# 门禁：API 正常、隧道连续 3 次不通，才算断开。没有通过就不能创建测试 Pod。
confirm_tunnel_down() {
  log "确认隧道已断"
  local deadline=$((SECONDS + ${1:-240})) fails=0 out rc
  while ((SECONDS < deadline)); do
    check_agent_pid
    out=$(tk "$T" --request-timeout=20s -n kube-system logs ds/konnectivity-agent --tail=1 2>&1); rc=$?
    if ! api_ok; then
      echo "$(date +%T) API 不可用，这次不计入"; fails=0
    elif ((rc != 0)); then
      fails=$((fails + 1)); echo "$(date +%T) 隧道不通 $fails/3: $out"
      ((fails >= 3)) && break
    else
      echo "$(date +%T) 隧道仍然通"; fails=0
    fi
    sleep 5
  done
  in_agent_ns iptables -L OUTPUT -v -n | grep -F "$LB_IP"
  echo "agent PID $(agent_pid)（阻断时 $AGENT_PID）"
  ((fails >= 3))
}

delete_probes() {
  tk "$T" delete pod "${PROBES[@]}" --ignore-not-found --force --grace-period=0 --request-timeout=30s >/dev/null 2>&1
  local deadline=$((SECONDS + 60))
  while ((SECONDS < deadline)) && [[ -n "$(tk "$T" get pod "${PROBES[@]}" --ignore-not-found -o name 2>/dev/null)" ]]; do sleep 2; done
  sleep 5  # 等 kubelet 回收设备
}

# 恢复到实验前的状态并检查实际效果。可重复运行。
restore_env() {
  log "恢复环境"
  local errors=0
  delete_probes
  unblock_tunnel
  [[ "$(webhook_policy)" == "$ORIG_POLICY" ]] || tk "$T" patch mutatingwebhookconfiguration "$WEBHOOK" --type=json \
    -p "[{\"op\":\"replace\",\"path\":\"/webhooks/0/failurePolicy\",\"value\":\"$ORIG_POLICY\"}]"
  tk "$T" delete validatingadmissionpolicybinding require-hami-scheduler --ignore-not-found
  tk "$T" delete validatingadmissionpolicy require-hami-scheduler --ignore-not-found
  wait_until 300 "隧道恢复" tunnel_ok || { echo "FAIL: 隧道没有恢复"; errors=$((errors + 1)); }
  local c
  fw_present && { echo "FAIL: agent 命名空间里还有阻断规则"; errors=$((errors + 1)); }
  for c in OUTPUT FORWARD; do node_fw_present "$c" && { echo "FAIL: 节点 $c 链还有阻断规则"; errors=$((errors + 1)); }; done
  [[ "$(webhook_policy)" == "$ORIG_POLICY" ]] || { echo "FAIL: webhook 策略没有恢复"; errors=$((errors + 1)); }
  vap_exists && { echo "FAIL: 准入策略还在"; errors=$((errors + 1)); }
  [[ -z "$(tk "$T" get pod "${PROBES[@]}" --ignore-not-found -o name 2>/dev/null)" ]] || { echo "FAIL: 探针 Pod 还在"; errors=$((errors + 1)); }
  local gpu; gpu=$(tk "$T" get node "$NODE" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}')
  [[ "$gpu" == 20 ]] || { echo "FAIL: nvidia.com/gpu=$gpu，期望 20"; errors=$((errors + 1)); }
  ((errors == 0)) && echo "环境已恢复：无阻断规则，策略=$ORIG_POLICY，无准入策略，无探针，gpu=$gpu"
  ((errors == 0))
}

# 创建全部探针，观察一段时间，把原始输出和一行汇总写到 $OUT。
# 用法: run_probes <实验> <对照|故障>
run_probes() {
  local exp="$1" group="$2" dir="$OUT/$2" pod t0 secs rc
  mkdir -p "$dir"
  declare -gA APPLY_RC=() APPLY_SECS=()
  log "$exp / $group：创建探针"
  for pod in "${PROBES[@]}"; do
    t0=$(date +%s.%N)
    tk "$T" apply -f "$EXP_DIR/$pod.yaml" --request-timeout=60s > "$dir/$pod.apply.txt" 2>&1; rc=$?
    secs=$(awk -v a="$t0" -v b="$(date +%s.%N)" 'BEGIN {printf "%.1f", b - a}')
    APPLY_RC[$pod]=$rc; APPLY_SECS[$pod]=$secs
    echo "$pod: rc=$rc ${secs}s: $(cat "$dir/$pod.apply.txt")"
  done
  echo "观察 ${OBSERVE}s"; sleep "$OBSERVE"
  tk "$T" get pods -o wide | tee "$dir/pods.txt"
  tk "$T" -n kube-system get pods -o wide > "$dir/kube-system-pods.txt" 2>&1
  for pod in "${PROBES[@]}"; do capture_probe "$exp" "$group" "$dir" "$pod"; done
}

capture_probe() {
  local exp="$1" group="$2" dir="$3" pod="$4" uid
  local msg; msg=$(tr '\n' ' ' < "$dir/$pod.apply.txt" | cut -c1-200)
  if ! tk "$T" get pod "$pod" -o json > "$dir/$pod.json" 2>/dev/null; then
    rm -f "$dir/$pod.json"
    row "$exp" "$group" "$pod" "${APPLY_RC[$pod]}" "${APPLY_SECS[$pod]}" 否 - - - - - "$msg"
    return
  fi
  tk "$T" describe pod "$pod" > "$dir/$pod.describe.txt" 2>&1
  uid=$(jq -r .metadata.uid "$dir/$pod.json")
  # Pod 重名时只取这一次的事件。
  tk "$T" get events --field-selector "involvedObject.uid=$uid" -o json > "$dir/$pod.events.json" 2>/dev/null
  local sched nodeName phase hami ev
  sched=$(jq -r .spec.schedulerName "$dir/$pod.json")
  nodeName=$(jq -r '.spec.nodeName // "-"' "$dir/$pod.json")
  phase=$(jq -r '[.status.phase, .status.reason, (.status.containerStatuses[]?.state.waiting.reason)] | map(select(. != null)) | join("/")' "$dir/$pod.json")
  hami=$(jq -r '[.metadata.annotations // {} | keys[] | select(test("hami|vgpu"))] | length' "$dir/$pod.json")
  ev=$(jq -r '[.items | sort_by(.lastTimestamp // .eventTime) | .[] | select(.type == "Warning")] | last | if . then "\(.reason): \(.message)" else "-" end' "$dir/$pod.events.json" 2>/dev/null | tr '|\n' '  ' | cut -c1-200)
  row "$exp" "$group" "$pod" "${APPLY_RC[$pod]}" "${APPLY_SECS[$pod]}" 是 "$sched" "$nodeName" "$phase" "$hami" "${ev:--}" "$msg"
}
row() {
  local IFS=$'\t'
  printf '%s\n' "$*" >> "$OUT_ROOT/probes.tsv"
  printf 'ROW %s\n' "$*"
}
# 这一轮记录里某个探针某一列的值。列: 4 rc，6 已创建，7 调度器，8 节点，9 状态，12 apply 输出。
col() { awk -F'\t' -v e="$1" -v g="$2" -v p="$3" -v c="$4" '$1==e && $2==g && $3==p {v=$c} END {print v}' "$OUT_ROOT/probes.tsv"; }

# 对照组不成立，故障组就没有比较意义。
check_control() {
  local e="$1" bad=0
  [[ "$(col "$e" 对照 over-card 7)" == hami-scheduler && "$(col "$e" 对照 over-card 8)" == - ]] \
    || { echo "对照组无效：over-card 应由 hami-scheduler 拒绝调度"; bad=1; }
  [[ "$(col "$e" 对照 gpu-only 7)" == hami-scheduler && "$(col "$e" 对照 gpu-only 9)" == Running ]] \
    || { echo "对照组无效：gpu-only 应由 hami-scheduler 调度并运行"; bad=1; }
  [[ "$(col "$e" 对照 plain-probe 9)" == Running ]] || { echo "对照组无效：plain-probe 应运行"; bad=1; }
  ((bad == 0)) && echo "对照组有效"
  ((bad == 0))
}

write_summary() {
  {
    echo "# Day 4-5 探针结果 $(basename "$OUT_ROOT")"; echo
    echo "| 实验 | 组 | 探针 | apply rc | 耗时(s) | 已创建 | 调度器 | 节点 | 状态 | HAMi 注解数 | 最后的 Warning | apply 输出 |"
    echo "|---|---|---|---|---|---|---|---|---|---|---|---|"
    awk -F'\t' '{printf "|"; for (i=1;i<=NF;i++) printf " %s |", $i; print ""}' "$OUT_ROOT/probes.tsv"
  } > "$OUT_ROOT/summary.md"
}
