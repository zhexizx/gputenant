#!/usr/bin/env bash
# 测简历数字 X：管理集群已就绪、镜像已构建时，N 次 `time gputenant create` 的中位数（README「简历数字」）。
# 每轮：确认租户不存在 → time create（从零开通）→ status 确认 14/14 → delete。结果写到 results/measure-x-<时间>/。
# 用法: measure-create.sh [租户名] [轮数]（默认 tenant-c、3 轮，t4×2）
source "$(dirname "$0")/lib.sh"
set +e
t="${1:-tenant-c}"; rounds="${2:-3}"
[[ "$t" != tenant-a && "$t" != tenant-b ]] || die "不要用 tenant-a、tenant-b：测完会删掉"
R="$ROOT/results/measure-x-$(date +%Y%m%d-%H%M%S)"; mkdir -p "$R"
go -C "$ROOT" build -o "$GPUTENANT_BIN" ./cmd/gputenant || die "构建失败"
commit=$(git -C "$ROOT" rev-parse --short HEAD)
[[ -n "$(git -C "$ROOT" status --porcelain -- cmd internal manifests)" ]] && commit="$commit-dirty"
{
  echo "# 测量环境 $(date -Is)"
  echo "代码版本: $commit"
  echo "租户: $t，GPU: t4×2，轮数: $rounds"
  docker info --format 'Docker: {{.ServerVersion}}，{{.OperatingSystem}}，{{.NCPU}} CPU，内存 {{.MemTotal}} 字节'
  echo "同时运行的容器: $(docker ps --format '{{.Names}}' | tr '\n' ' ')"
  echo "已有租户: $(kubectl -n tenants get tcp -o jsonpath='{.items[*].metadata.name}')"
} > "$R/env.txt"

secs=()
for i in $(seq "$rounds"); do
  # 每轮从零开始：上一轮没删干净就先删
  if kubectl -n tenants get tcp "$t" >/dev/null 2>&1 || docker ps -a --format '{{.Names}}' | grep -q "^$t-"; then
    "$GPUTENANT_BIN" delete "$t" > "$R/pre-delete-$i.log" 2>&1
  fi
  start=$(date +%s.%N)
  "$GPUTENANT_BIN" create "$t" --gpu-profile t4 --gpu-count 2 > "$R/create-$i.log" 2>&1; rc=$?
  end=$(date +%s.%N)
  s=$(awk -v a="$start" -v b="$end" 'BEGIN {printf "%.1f", b - a}')
  "$GPUTENANT_BIN" status "$t" > "$R/status-$i.log" 2>&1; src=$?
  skipped=$(grep -c ' skipped$' "$R/create-$i.log")
  echo "round $i: create rc=$rc, ${s}s, status rc=$src, skipped=$skipped" | tee -a "$R/rounds.txt"
  if ((rc == 0 && src == 0)); then secs+=("$s"); fi
  "$GPUTENANT_BIN" delete "$t" > "$R/delete-$i.log" 2>&1
done

{
  echo; echo "# 结果"
  echo "成功轮数: ${#secs[@]}/$rounds"
  if ((${#secs[@]} == rounds)); then
    sorted=$(printf '%s\n' "${secs[@]}" | sort -n)
    echo "各轮（秒，排序后）: $(echo "$sorted" | xargs printf '%.1f ')"
    med=$(echo "$sorted" | awk '{a[NR]=$1} END {print (NR%2 ? a[(NR+1)/2] : (a[NR/2]+a[NR/2+1])/2)}')
    printf '中位数 X = %.1f 秒（%d 分 %02d 秒）\n' "$med" "$(printf '%.0f' "$med" | awk '{print int($1/60)}')" "$(printf '%.0f' "$med" | awk '{print $1%60}')"
  else
    echo "有失败的轮次，不计算 X"
  fi
} | tee -a "$R/rounds.txt"
echo "$R"
