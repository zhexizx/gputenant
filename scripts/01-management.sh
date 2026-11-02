#!/usr/bin/env bash
# 管理集群：kind、cert-manager、MetalLB、Kamaji。可重复运行。
source "$(dirname "$0")/lib.sh"
mkdir -p "$KUBECONFIG_DIR"

if ! kind get clusters 2>/dev/null | grep -qx "$MGMT_CLUSTER"; then
  log "创建 kind 集群 $MGMT_CLUSTER"
  kind create cluster --name "$MGMT_CLUSTER" --kubeconfig "$MGMT_KUBECONFIG" --wait 120s
fi
kind get kubeconfig --name "$MGMT_CLUSTER" > "$MGMT_KUBECONFIG"
chmod 600 "$MGMT_KUBECONFIG"
kubectl get nodes

log "cert-manager $CERT_MANAGER_VERSION"
kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/$CERT_MANAGER_VERSION/cert-manager.yaml"
kubectl -n cert-manager rollout status deploy --timeout=300s

log "MetalLB $METALLB_VERSION"
kubectl apply -f "https://raw.githubusercontent.com/metallb/metallb/$METALLB_VERSION/config/manifests/metallb-frr-k8s.yaml"
kubectl -n metallb-system rollout status deploy --timeout=300s
kubectl -n metallb-system rollout status ds --timeout=300s

subnet=$(docker network inspect kind | jq -r '.[0].IPAM.Config[].Subnet | select(test("^[0-9.]+/"))' | head -1)
[[ "$subnet" == */16 ]] || die "kind 子网不是 /16：'$subnet'，需要调整地址池计算"
prefix=$(cut -d. -f1,2 <<<"$subnet")
pool="$prefix.255.200-$prefix.255.250"
echo "kind 子网 $subnet，MetalLB 地址池 $pool"
cat <<EOF | kubectl apply -f -
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: kind-ip-pool
  namespace: metallb-system
spec:
  addresses:
    - $pool
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: empty
  namespace: metallb-system
EOF

log "Kamaji（仓库内的 chart，镜像固定 digest）"
echo "$KAMAJI_CHART_SHA256  $KAMAJI_CHART" | sha256sum -c -
kamaji_args=(--set 'resources=null' --set "image.tag=$KAMAJI_IMAGE_TAG")
helm template kamaji "$KAMAJI_CHART" -n kamaji-system "${kamaji_args[@]}" | grep -qF "clastix/kamaji:$KAMAJI_IMAGE_TAG" \
  || die "渲染结果里没有固定 digest 的 Kamaji 镜像，检查 chart 的键名"
helm upgrade --install kamaji "$KAMAJI_CHART" \
  --namespace kamaji-system --create-namespace \
  "${kamaji_args[@]}" --wait --timeout 8m
kubectl -n kamaji-system get pods
kubectl get datastores
