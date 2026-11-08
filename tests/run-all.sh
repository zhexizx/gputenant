#!/usr/bin/env bash
# 跑 TC-00~09，结果写到 results/<时间>/。退出码为 0 且日志里有 RESULT ... PASS，才算通过。
# TC-00 是测试框架自检，不需要集群。用法: run-all.sh [00 01 ...]（默认全部）
source "$(dirname "$0")/lib.sh"
RUN_DIR="$ROOT/results/$(date +%Y%m%d-%H%M%S)"; export RUN_DIR
mkdir -p "$RUN_DIR"
ids=("$@"); ((${#ids[@]})) || ids=(00 01 02 03 04 05 06 07 08 09)

# 记录测试用的代码版本；工作区有未提交的改动时标记 -dirty。
commit=$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)
[[ -n "$(git -C "$ROOT" status --porcelain -- scripts tests manifests 2>/dev/null)" ]] && commit="$commit-dirty"

# 每次从干净状态开始，避免上一轮的 Pod 让测试假通过。
if tk "$A" get --raw /readyz >/dev/null 2>&1; then
  tk "$A" delete pod gpu-share-1 gpu-share-2 gpu-share-3 gpu-share-4 gpu-mem-big --ignore-not-found --wait=true >/dev/null
fi

{
  echo "# TC 结果 $(basename "$RUN_DIR")"; echo
  echo "代码版本：\`$commit\`"; echo
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
