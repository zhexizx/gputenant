#!/usr/bin/env bash
# 一次搭好环境。用法: up.sh [tenant-a] [tenant-b]（默认两个都建）
source "$(dirname "$0")/lib.sh"
set -o pipefail  # tee 不能掩盖某一步的失败
here="$(dirname "$0")"
tenants=("$@"); ((${#tenants[@]})) || tenants=(tenant-a tenant-b)
for t in "${tenants[@]}"; do check_tenant "$t"; done

mkdir -p "$ROOT/results/env"
step() { local name="$1"; shift; "$@" 2>&1 | tee "$ROOT/results/env/$name.log"; }
step 00-tools "$here/00-tools.sh"
step 00-preflight "$here/00-preflight.sh"
step 00-build-images "$here/00-build-images.sh"
step 01-management "$here/01-management.sh"
for t in "${tenants[@]}"; do
  step "02-$t-cp" "$here/02-tenant-cp.sh" "$t"
  step "03-$t-worker" "$here/03-tenant-worker.sh" "$t"
  step "04-$t-verify" "$here/04-verify-env.sh" "$t"
done
