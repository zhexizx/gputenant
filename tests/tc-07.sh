#!/usr/bin/env bash
source "$(dirname "$0")/lib.sh"
tc_begin TC-07 "显存超分被拒绝（申请 50000，单卡 40960）"
run tk "$A" get node "$NODE_A" -o jsonpath='{.metadata.annotations.hami\.io/node-nvidia-register}{"\n"}'
check_eq "单卡显存和计划一致" "$(tk "$A" get node "$NODE_A" -o json | jq -r '.metadata.annotations["hami.io/node-nvidia-register"] | fromjson | .[0].devmem')" 40960
tk "$A" delete pod gpu-mem-big --ignore-not-found --wait=true
gpu_pod gpu-mem-big 50000 10
wait_event gpu-mem-big 'CardInsufficientMemory|FailedScheduling' 60
# 事件出现后再等 20 秒，确认 Pod 没有在重试时被调度。
sleep 20
run tk "$A" get pod gpu-mem-big -o wide
tk "$A" describe pod gpu-mem-big | sed -n '/Events/,$p'
check_pending_on_hami gpu-mem-big
check "事件包含 CardInsufficientMemory" wait_event gpu-mem-big 'CardInsufficientMemory' 5
tc_end
