#!/usr/bin/env bash
# 按顺序跑：环境检查 → 基线 → 方案 A → 方案 B → webhook 故障 → 告警。任何一步失败就停止。
source "$(dirname "$0")/lib.sh"
mkdir -p "$OUT_ROOT"
exec > >(tee "$OUT_ROOT/run.log") 2>&1
echo "结果目录: $OUT_ROOT"
exec 9>"$CACHE_DIR/experiment.lock"
flock -n 9 || abort "另一个实验正在运行"
export LAB_LOCK_HELD=1

# 04-verify-env.sh 会被 guard 的进程检查匹配，所以放在最前面同步跑完。
"$ROOT/scripts/04-verify-env.sh" "$T" > "$OUT_ROOT/verify-env.txt" 2>&1 || { cat "$OUT_ROOT/verify-env.txt"; abort "环境验证失败"; }
tail -1 "$OUT_ROOT/verify-env.txt"

overall=0
for step in "degrade.sh baseline" "degrade.sh fix-a" "degrade.sh fix-b" "webhook-down.sh" "alert.sh"; do
  log "=== $step"
  # shellcheck disable=SC2086
  bash "$ROOT/experiments/"$step; rc=$?
  echo "$step: rc=$rc" | tee -a "$OUT_ROOT/steps.txt"
  if ((rc != 0)); then overall=1; break; fi
done

leaks=$(grep -rlE 'client-key-data|client-certificate-data|BEGIN (RSA |EC )?PRIVATE KEY|--discovery-token-ca-cert-hash' "$OUT_ROOT" || true)
if [[ -n "$leaks" ]]; then echo "凭证扫描失败: $leaks"; overall=1; fi
echo; cat "$OUT_ROOT/steps.txt"
[[ -f "$OUT_ROOT/summary.md" ]] && cat "$OUT_ROOT/summary.md"
echo "结果目录: $OUT_ROOT"
echo "overall=$overall"
exit "$overall"
