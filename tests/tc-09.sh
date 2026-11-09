#!/usr/bin/env bash
# 默认防护：每个租户都部署了准入策略和告警，并且准入策略真的生效。
# 用 --dry-run=server，不创建 Pod。带 hami.io/webhook=ignore 标签的 Pod 会跳过 HAMi webhook，
# schedulerName 保持 default-scheduler，和隧道断开、webhook 挂掉时的效果一样，不用真的制造故障。
source "$(dirname "$0")/lib.sh"
tc_begin TC-09 "默认防护：准入策略和告警"

# probe_pod <名称> <skip|webhook> <containers|init|none>：生成探测 Pod。
probe_pod() {
  local labels="" gpu_in="$3"
  [[ "$2" == skip ]] && labels=$'  labels:\n    hami.io/webhook: ignore'
  local gpu=$'      resources:\n        limits:\n          nvidia.com/gpu: 1'
  cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
$labels
spec:
EOF
  if [[ "$gpu_in" == init ]]; then
    printf '  initContainers:\n    - name: init\n      image: %s\n      command: ["true"]\n%s\n' "$BUSYBOX" "$gpu"
  fi
  printf '  containers:\n    - name: app\n      image: %s\n      command: ["sleep", "infinity"]\n' "$BUSYBOX"
  [[ "$gpu_in" == containers ]] && printf '%s\n' "$gpu"
  return 0
}
# dry_run <租户> <名称> <skip|webhook> <containers|init|none>：输出 "rc|schedulerName 或报错"。
dry_run() {
  local out rc
  out=$(probe_pod "$2" "$3" "$4" | tk "$1" create --dry-run=server -f - -o jsonpath='{.spec.schedulerName}' 2>&1); rc=$?
  printf '\n$ kubectl create --dry-run=server（%s，webhook=%s，GPU 在 %s）\n%s\n' "$2" "$3" "$4" "$out" >&2
  echo "$rc|$out"
}
DENY='GPU Pod must use hami-scheduler'

# CI 只建 tenant-a，用 TC_TENANTS 指定；默认两个租户都测。
for t in ${TC_TENANTS:-$A $B}; do
  echo; echo "===== $t ====="
  run tk "$t" get validatingadmissionpolicy,validatingadmissionpolicybinding require-hami-scheduler
  check "$t 有准入策略和绑定" tk "$t" get validatingadmissionpolicybinding require-hami-scheduler

  r=$(dry_run "$t" tc09-gpu webhook containers)
  check_eq "$t 正常的 GPU Pod：允许，交给 hami-scheduler" "$r" "0|hami-scheduler"
  r=$(dry_run "$t" tc09-init webhook init)
  check_eq "$t 正常的 init 容器 GPU Pod：允许，交给 hami-scheduler" "$r" "0|hami-scheduler"
  r=$(dry_run "$t" tc09-skip-gpu skip containers)
  check "$t 跳过 webhook 的 GPU Pod：被拒绝" grep -qF "$DENY" <<<"$r"
  r=$(dry_run "$t" tc09-skip-init skip init)
  check "$t 跳过 webhook、只在 init 容器申请 GPU 的 Pod：被拒绝" grep -qF "$DENY" <<<"$r"
  r=$(dry_run "$t" tc09-skip-plain skip none)
  check_eq "$t 跳过 webhook 的普通 Pod：允许" "$r" "0|default-scheduler"

  printf '\n$ Prometheus 规则和告警（%s）\n' "$t"
  prom_api "$t" rules | jq -r '.data.groups[].rules[] | "\(.name) \(.health) \(.state)"'
  check "$t 告警规则已加载：${GUARDRAIL_ALERTS[*]}" prom_rules_loaded "$t"
  check "$t 告警依赖的信号都是正常值" prom_signals_ok "$t"
  check "$t 没有 firing 的告警" prom_none_firing "$t"
done
tc_end
