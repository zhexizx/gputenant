#!/usr/bin/env bash
# 删除环境、从零重建、跑全部测试，确认结果不依赖手工改动。会删除本实验的集群和节点。
source "$(dirname "$0")/lib.sh"
here="$(dirname "$0")"
if kind get clusters 2>/dev/null | grep -qx "$MGMT_CLUSTER"; then "$here/down.sh"; fi
"$here/up.sh"
exec "$ROOT/tests/run-all.sh"
