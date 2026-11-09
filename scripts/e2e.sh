#!/usr/bin/env bash
# 本地 e2e：用 CLI 开通租户（已建好的步骤会跳过）、gputenant verify 冒烟，再跑 TC。
# 管理集群和镜像要先就绪（00-build-images.sh、01-management.sh，或者直接 up.sh）。
# 用法: [E2E_TENANTS="tenant-a"] e2e.sh [TC 编号...]（默认两个租户、TC-00~09）
# CI 只建 tenant-a：E2E_TENANTS=tenant-a e2e.sh 00 01 02 03 04 05 06 07 09（TC-08 要两个租户）
source "$(dirname "$0")/lib.sh"
set -o pipefail
RUN_DIR="$ROOT/results/e2e-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RUN_DIR"
step() { local name="$1"; shift; log "$name"; "$@" 2>&1 | tee "$RUN_DIR/$name.log"; }

step build-cli go -C "$ROOT" build -o "$GPUTENANT_BIN" ./cmd/gputenant || die "构建失败"
read -r -a tenants <<<"${E2E_TENANTS:-tenant-a tenant-b}"
for t in "${tenants[@]}"; do check_tenant "$t"; done
for t in "${tenants[@]}"; do
  # shellcheck disable=SC2046
  step "create-$t" "$GPUTENANT_BIN" create "$t" $(tenant_gpu_args "$t") || die "$t 开通失败"
  step "verify-$t" "$GPUTENANT_BIN" verify "$t" || die "$t 冒烟测试失败"
  step "status-$t" "$GPUTENANT_BIN" status "$t" || die "$t status 不正常"
done
log "TC，结果在 results/<时间>/"
TC_TENANTS="${tenants[*]}" "$ROOT/tests/run-all.sh" "$@"
