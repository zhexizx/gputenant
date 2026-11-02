#!/usr/bin/env bash
source "$(dirname "$0")/lib.sh"
tc_begin TC-01 "控制面和 API 地址"
run kubectl -n tenants get tcp,pods,svc
for t in ${TC_TENANTS:-$A $B}; do
  echo; echo "--- $t"
  check_eq "$t TCP 状态" "$(kubectl -n tenants get tcp "$t" -o jsonpath='{.status.kubernetesResources.version.status}')" Ready
  check_eq "$t 控制面 Pod 4 个容器都 Ready" \
    "$(kubectl -n tenants get pods -l kamaji.clastix.io/name="$t" -o json | jq '[.items[].status.containerStatuses[] | select(.ready)] | length')" 4
  ip=$(kubectl -n tenants get svc "$t" -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
  check "$t LB IP 已分配 ($ip)" test -n "$ip"
  # 从 kind 网络内部直连 LB IP，不经过本地代理。
  code=$(docker exec "$MGMT_CLUSTER-control-plane" curl -sk -m 10 -o /dev/null -w '%{http_code}' "https://$ip:6443/readyz")
  check_eq "$t 从 kind 网络访问 /readyz" "$code" 200
  check_eq "$t kubeconfig 访问 /readyz" "$(tk "$t" get --raw /readyz)" ok
done
tc_end
