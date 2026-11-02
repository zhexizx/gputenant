#!/usr/bin/env bash
source "$(dirname "$0")/lib.sh"
tc_begin TC-05 "3 个 Pod 共享一张卡"
ensure_share_pods
run tk "$A" get pods -o wide
printf '\n$ kubectl describe node | grep nvidia.com/gpu\n'
tk "$A" describe node "$NODE_A" | grep nvidia.com/gpu
uuids=()
for i in 1 2 3; do
  check_eq "gpu-share-$i Running" "$(tk "$A" get pod "gpu-share-$i" -o jsonpath='{.status.phase}')" Running
  check_eq "gpu-share-$i 所在节点" "$(tk "$A" get pod "gpu-share-$i" -o jsonpath='{.spec.nodeName}')" "$NODE_A"
  a=$(tk "$A" get pod "gpu-share-$i" -o jsonpath='{.metadata.annotations.hami\.io/vgpu-devices-allocated}')
  uuids+=("$(grep -oE 'GPU-[0-9a-f-]+' <<<"$a" | head -1)")
  echo "gpu-share-$i -> ${uuids[-1]}"
done
check "分配到了 GPU UUID" test -n "${uuids[0]}"
check_eq "分配到同一张卡" "$(printf '%s\n' "${uuids[@]}" | sort -u | wc -l)" 1
tc_end
