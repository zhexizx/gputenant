#!/usr/bin/env bash
# 租户控制面：TenantControlPlane + 导出 kubeconfig。可重复运行。
# 用法: 02-tenant-cp.sh tenant-a|tenant-b
source "$(dirname "$0")/lib.sh"
t="${1:-}"; check_tenant "$t"

kubectl create namespace tenants --dry-run=client -o yaml | kubectl apply -f -

log "TenantControlPlane $t"
# 不启用 kubeProxy addon，由 03-tenant-worker.sh 自管（见 troubleshooting 第 3 条）。
cat <<EOF | kubectl apply -f -
apiVersion: kamaji.clastix.io/v1alpha1
kind: TenantControlPlane
metadata:
  name: ${t}
  namespace: tenants
  labels:
    tenant.clastix.io: ${t}
spec:
  controlPlane:
    deployment:
      replicas: 1
    service:
      serviceType: LoadBalancer
  kubernetes:
    version: "${K8S_VERSION}"
    kubelet:
      configurationJSONPatches:
        - op: add
          path: /cgroupDriver
          value: systemd
        # 和 kind 自己的节点配置一致：Docker Desktop VM 开了 swap，默认 true 时 kubelet 起不来。
        - op: add
          path: /failSwapOn
          value: false
  networkProfile:
    port: 6443
    podCidr: ${POD_CIDR}
  addons:
    coreDNS: {}
    konnectivity:
      server:
        port: 8132
      agent:
        mode: DaemonSet
EOF

tcp_ready() {
  [[ "$(kubectl -n tenants get tcp "$t" -o jsonpath='{.status.kubernetesResources.version.status}')" == Ready ]]
}
if ! wait_until 480 "$t 控制面 Ready" tcp_ready; then
  kubectl -n tenants get tcp,pods,svc
  kubectl -n tenants describe tcp "$t" | tail -30
  kubectl -n tenants get events --sort-by=.lastTimestamp | tail -20
  exit 1
fi
kubectl -n tenants rollout status "deploy/$t" --timeout=300s
kubectl -n tenants get tcp,pods,svc

# 启用 addon 后 Kamaji 会管理 kube-proxy，并还原对它的修改。
if [[ -n "$(kubectl -n tenants get tcp "$t" -o jsonpath='{.spec.addons.kubeProxy}')" ]]; then
  die "$t 启用了 kubeProxy addon，见 troubleshooting 第 3 条"
fi

log "导出 $t kubeconfig"
# internal: 证书原样，地址是 LB IP，给 kind 网络内的工作节点用。
# 本地: 经 socat 容器走 127.0.0.1。Docker Desktop 下 WSL 没有到 kind 网桥的路由。
umask 077
mkdir -p "$GPUTENANT_HOME/$t"
kc="$(tenant_kubeconfig "$t")"
internal="$(tenant_internal_kubeconfig "$t")"
kubectl -n tenants get secret "${t}-admin-kubeconfig" -o jsonpath='{.data.admin\.conf}' | base64 -d > "$internal"

lb_ip=$(kubectl -n tenants get svc "$t" -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
proxy="${t}-api-proxy"
port=$(tenant_api_port "$t" 2>/dev/null || true)
[[ -n "$port" ]] || case "$t" in tenant-a) port=16443 ;; tenant-b) port=16444 ;; esac
if [[ "$(docker inspect -f '{{index .Config.Cmd 1}}' "$proxy" 2>/dev/null || true)" != "TCP:$lb_ip:6443" ]]; then
  docker rm -f "$proxy" >/dev/null 2>&1 || true
  docker run -d --name "$proxy" --restart unless-stopped --network kind \
    -p "127.0.0.1:$port:6443" "$SOCAT_IMAGE" TCP-LISTEN:6443,fork,reuseaddr "TCP:$lb_ip:6443" >/dev/null
fi

cluster=$(kubectl --kubeconfig "$internal" config view -o jsonpath='{.clusters[0].name}')
cp "$internal" "$kc"
kubectl --kubeconfig "$kc" config set-cluster "$cluster" \
  --server "https://127.0.0.1:$port" --tls-server-name "$lb_ip" >/dev/null
wait_until 120 "$t apiserver 可访问" kubectl --kubeconfig "$kc" get --raw /readyz
kubectl --kubeconfig "$kc" cluster-info
