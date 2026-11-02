#!/usr/bin/env bash
source "$(dirname "$0")/lib.sh"
tc_begin TC-08 "租户隔离"
for t in "$A" "$B"; do
  echo; echo "===== $t ====="
  run tk "$t" get nodes
  printf '\n$ kubectl get pods -A | grep gpu-\n'; tk "$t" get pods -A | grep -E 'gpu-|NAME'
  tk "$t" get nodes -o json | jq '.items[] | {name: .metadata.name, gpu: .status.allocatable["nvidia.com/gpu"], register: .metadata.annotations["hami.io/node-nvidia-register"]}'
done
echo
check_eq "$A 只看到自己的节点" "$(tk "$A" get nodes -o jsonpath='{.items[*].metadata.name}')" tenant-a-worker
check_eq "$B 只看到自己的节点" "$(tk "$B" get nodes -o jsonpath='{.items[*].metadata.name}')" tenant-b-worker
check "$A 能看到 gpu-share-1" tk "$A" get pod gpu-share-1
check "$B 的 API 可访问" tk "$B" get --raw /readyz
if fetch b_pods "$B" get pods -A; then
  check_eq "$B 看不到 $A 的 gpu-* Pod" "$(jq '[.items[].metadata.name | select(test("gpu-"))] | length' <<<"$b_pods")" 0
fi
check_eq "$A nvidia.com/gpu" "$(tk "$A" get node tenant-a-worker -o jsonpath='{.status.allocatable.nvidia\.com/gpu}')" 10
check_eq "$B nvidia.com/gpu" "$(tk "$B" get node tenant-b-worker -o jsonpath='{.status.allocatable.nvidia\.com/gpu}')" 20
check_eq "$B 有 2 张卡" "$(tk "$B" get node tenant-b-worker -o json | jq -r '.metadata.annotations["hami.io/node-nvidia-register"] | fromjson | length')" 2
# 凭证隔离：用 A 的客户端证书访问 B 的 API，必须被拒绝。先确认 B 可达，否则连不上也会“通过”。
cross=$(kubectl --kubeconfig "$(tenant_kubeconfig "$A")" --server "https://127.0.0.1:$(tenant_api_port "$B")" \
  --insecure-skip-tls-verify=true get nodes 2>&1); rc=$?
echo "A 的凭证访问 B: rc=$rc, $cross"
check "A 的凭证被 B 的 API 拒绝（401/403）" grep -qE 'Unauthorized|Forbidden|must be logged in' <<<"$cross"
tc_end
