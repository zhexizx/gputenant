#!/usr/bin/env bash
source "$(dirname "$0")/lib.sh"
tc_begin TC-04 "HAMi 完成分配和绑定，节点锁自动释放"
ensure_share_pods
printf '\n$ kubectl describe pod gpu-share-1 | grep ...\n'
tk "$A" describe pod gpu-share-1 | grep -E 'vgpu-devices-allocated|BindingSucceed|Successfully'
for i in 1 2 3; do
  check_eq "gpu-share-$i bind-phase" "$(tk "$A" get pod "gpu-share-$i" -o jsonpath='{.metadata.annotations.hami\.io/bind-phase}')" success
  alloc=$(tk "$A" get pod "gpu-share-$i" -o jsonpath='{.metadata.annotations.hami\.io/vgpu-devices-allocated}')
  echo "gpu-share-$i allocated: $alloc"
  check "gpu-share-$i 有分配注解" test -n "$alloc"
done
printf '\n$ 节点上的 mutex/lock 注解（应该为空）\n'
locks=$(tk "$A" get node "$NODE_A" -o json | jq -r '.metadata.annotations | keys[] | select(test("mutex|lock"))')
echo "${locks:-<无>}"
check_eq "节点锁已释放，且没有手动解锁" "$locks" ""
tc_end
