#!/usr/bin/env bash
# 跑 TC-01~08，结果写到 results/<时间>/，只根据每个 TC 的退出码生成 summary.md。
# 用法: run-all.sh [01 02 ...]（默认全部）
source "$(dirname "$0")/lib.sh"
RUN_DIR="$ROOT/results/$(date +%Y%m%d-%H%M%S)"; export RUN_DIR
mkdir -p "$RUN_DIR"
ids=("$@"); ((${#ids[@]})) || ids=(01 02 03 04 05 06 07 08)

# 每次从干净状态开始，避免上一轮的 Pod 让测试假通过。
if tk "$A" get --raw /readyz >/dev/null 2>&1; then
  tk "$A" delete pod gpu-share-1 gpu-share-2 gpu-share-3 gpu-share-4 gpu-mem-big --ignore-not-found --wait=true >/dev/null
fi

{
  echo "# TC 结果 $(basename "$RUN_DIR")"; echo
  echo "| TC | 结论 | 说明 |"; echo "|---|---|---|"
} > "$RUN_DIR/summary.md"
overall=0
for id in "${ids[@]}"; do
  bash "$ROOT/tests/tc-$id.sh"; rc=$?
  sleep 1  # 等 tee 把日志写完
  f="$RUN_DIR/TC-$id.log"
  if [[ $rc -eq 0 ]] && grep -q "^RESULT TC-$id PASS" "$f"; then v=PASS; else v=FAIL; overall=1; fi
  echo "| TC-$id | $v | $(sed -n '1s/^# TC-[0-9]*: //p' "$f"); $(grep -E '^RESULT' "$f" | sed -E 's/.*\((.*)\)/\1/') |" >> "$RUN_DIR/summary.md"
done

# 结果里不能有凭证。
leaks=$(grep -rlE 'client-key-data|client-certificate-data|BEGIN (RSA |EC )?PRIVATE KEY|--discovery-token-ca-cert-hash' "$RUN_DIR" || true)
if [[ -n "$leaks" ]]; then
  printf '\n**凭证扫描失败：** %s\n' "$leaks" >> "$RUN_DIR/summary.md"; overall=1
fi
echo; cat "$RUN_DIR/summary.md"
exit $overall
