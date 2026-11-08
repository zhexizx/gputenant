#!/usr/bin/env bash
# 只删除本实验的资源：kamaji 集群、租户节点和代理容器。不动其他 kind 集群。
source "$(dirname "$0")/lib.sh"
for t in tenant-a tenant-b; do
  docker rm -fv "$t-worker" "$t-api-proxy" 2>/dev/null || true
done
kind delete cluster --name "$MGMT_CLUSTER" --kubeconfig "$MGMT_KUBECONFIG"
rm -f "${KUBECONFIG_DIR:?}"/*.kubeconfig
rm -rf "${GPUTENANT_HOME:?}"/tenant-a "${GPUTENANT_HOME:?}"/tenant-b
