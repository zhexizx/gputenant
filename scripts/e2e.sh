#!/usr/bin/env bash
# 本地 e2e：用 CLI 开通租户（已建好的步骤会跳过）、gputenant verify 冒烟，再跑 TC。
# 管理集群和镜像要先就绪（00-build-images.sh、01-management.sh，或者直接 up.sh）。
# 用法: e2e.sh [TC 编号...]（默认 TC-00~09，两个租户）
source "$(dirname "$0")/lib.sh"
set -o pipefail
RUN_DIR="$ROOT/results/e2e-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RUN_DIR"
step() { local name="$1"; shift; log "$name"; "$@" 2>&1 | tee "$RUN_DIR/$name.log"; }

step build-cli go -C "$ROOT" build -o "$GPUTENANT_BIN" ./cmd/gputenant || die "构建失败"
for t in tenant-a tenant-b; do
  # shellcheck disable=SC2046
  step "create-$t" "$GPUTENANT_BIN" create "$t" $(tenant_gpu_args "$t") || die "$t 开通失败"
  step "verify-$t" "$GPUTENANT_BIN" verify "$t" || die "$t 冒烟测试失败"
  step "status-$t" "$GPUTENANT_BIN" status "$t" || die "$t status 不正常"
done
log "TC，结果在 results/<时间>/"
"$ROOT/tests/run-all.sh" "$@"
