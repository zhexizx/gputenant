#!/usr/bin/env bash
source "$(dirname "$0")/lib.sh"
tc_begin TC-06 "算力超分被拒绝（已用 90，再申请 30）"
ensure_share_pods
tk "$A" delete pod gpu-share-4 --ignore-not-found --wait=true
gpu_pod gpu-share-4 1000 30
wait_event gpu-share-4 'CardInsufficient|FilteringFailed|FailedScheduling' 60
# 事件出现后再等 20 秒，确认 Pod 没有在重试时被调度。
sleep 20
run tk "$A" get pod gpu-share-4 -o wide
tk "$A" describe pod gpu-share-4 | sed -n '/Events/,$p'
check_pending_on_hami gpu-share-4
check "事件给出算力不足的原因" wait_event gpu-share-4 'CardInsufficientCore|CardComputeUnitsExhausted' 5
tc_end
