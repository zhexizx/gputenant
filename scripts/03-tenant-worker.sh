#!/usr/bin/env bash
# 工作节点、Calico、HAMi、mock GPU。可重复运行。
# 用法: 03-tenant-worker.sh tenant-a|tenant-b
source "$(dirname "$0")/lib.sh"
t="${1:-}"; check_tenant "$t"
node="${t}-worker"
kc="$(tenant_kubeconfig "$t")"
[[ -f "$kc" ]] || die "没有 $kc，先运行 02-tenant-cp.sh $t"
mkdir -p "$CACHE_DIR"

log "工作节点 $node"
if ! docker inspect "$node" >/dev/null 2>&1; then
  docker run -d --name "$node" --hostname "$node" \
    --network kind --privileged \
    --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
    --tmpfs /tmp --tmpfs /run -v /var -v /lib/modules:/lib/modules:ro \
    "$NODE_IMAGE" >/dev/null
fi
wait_until 60 "$node systemd 启动" docker exec "$node" systemctl is-system-running --wait

# 节点有 swap 时必须已经下发 failSwapOn: false，否则 join 要白等 4 分钟才失败。
swap_kb=$(docker exec "$node" awk '/SwapTotal/ {print $2}' /proc/meminfo)
fail_swap=$(tk "$t" -n kube-system get cm kubelet-config -o jsonpath='{.data.kubelet}' | awk '/^failSwapOn:/ {print $2}')
echo "节点 swap=${swap_kb}kB，kubelet failSwapOn=${fail_swap:-默认 true}"
if ((swap_kb > 0)) && [[ "$fail_swap" != false ]]; then
  die "节点有 swap，但 kubelet-config 没有 failSwapOn: false，见 troubleshooting 第 2 条"
fi

if ! tk "$t" get node "$node" >/dev/null 2>&1; then
  docker exec -i "$node" sh -c "umask 077; cat > /root/$t.kubeconfig" < "$(tenant_internal_kubeconfig "$t")"
  join_cmd=$(docker exec "$node" kubeadm token create --print-join-command --kubeconfig "/root/$t.kubeconfig")
  # 不把 join 命令打到日志里：里面有 bootstrap token。
  docker exec "$node" bash -c "$join_cmd --ignore-preflight-errors=all"
  docker exec "$node" rm -f "/root/$t.kubeconfig"
fi
wait_until 60 "$node 已注册" tk "$t" get node "$node"

log "kube-proxy（自管，--conntrack-max-per-core=0）"
lb_ip=$(kubectl -n tenants get svc "$t" -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
sed -e "s|\${LB_IP}|$lb_ip|g" -e "s|\${K8S_VERSION}|$K8S_VERSION|g" -e "s|\${POD_CIDR}|$POD_CIDR|g" \
  "$ROOT/manifests/kube-proxy.yaml" | tk "$t" apply -f -
# rollout status 可能在崩溃前就报成功，所以直接看 ClusterIP 规则。
kube_proxy_ok() { docker exec "$node" iptables -t nat -S KUBE-SERVICES | grep -q 'default/kubernetes'; }
wait_until 120 "kube-proxy 规则已写入" kube_proxy_ok

log "Calico $CALICO_VERSION"
calico="$CACHE_DIR/calico-$CALICO_VERSION.yaml"
[[ -s "$calico" ]] || curl -fsSL "https://raw.githubusercontent.com/projectcalico/calico/$CALICO_VERSION/manifests/calico.yaml" -o "$calico"
# 缓存文件可能已被改过，所以注释状态和已有的值都要能处理。
sed -i "/- name: CALICO_IPV4POOL_CIDR/{s|# - name|- name|;n;s|#   value|  value|;s|value: .*|value: \"$POD_CIDR\"|}" "$calico"
grep -A1 CALICO_IPV4POOL_CIDR "$calico"
grep -qF "value: \"$POD_CIDR\"" "$calico" || die "Calico manifest 没有写入 POD_CIDR=$POD_CIDR"
tk "$t" apply -f "$calico"
tk "$t" wait --for=condition=Ready "node/$node" --timeout=300s
tk "$t" -n kube-system rollout status ds/calico-node --timeout=300s
# 等 Pod 而不是 rollout status：节点晚于 Deployment 就绪时，进度时限可能已经过期。
tk "$t" -n kube-system wait pod -l k8s-app=calico-kube-controllers --for=condition=Ready --timeout=300s

log "导入本地镜像"
docker save nvml-mock:local hami:local | docker exec -i "$node" ctr -n k8s.io images import -

log "nvml-mock"
case "$t" in
  tenant-a) profile=a100; count=1; expected_gpu=10 ;;
  tenant-b) profile=t4;   count=2; expected_gpu=20 ;;
esac
nvml_chart=oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock
nvml_args=(--version "$NVML_MOCK_CHART_VERSION"
  --set image.repository=nvml-mock --set image.tag=local
  --set gpu.profile="$profile" --set gpu.count="$count")
# Helm 会静默忽略不存在的键，所以安装前先看渲染出来的镜像。
"$ROOT/scripts/check-helm-render.sh" nvml-mock "$nvml_chart" "${nvml_args[@]}"
helm upgrade --install nvml-mock "$nvml_chart" --kubeconfig "$kc" "${nvml_args[@]}" \
  --wait --timeout 180s >/dev/null  # chart 的 NOTES 很长，这里不打印
helm list --kubeconfig "$kc"
# chart 0.6.0 不会自动打这个标签，它的 NOTES 要求手动打。
tk "$t" label node "$node" nvidia.com/gpu.present=true --overwrite

log "HAMi @$HAMI_COMMIT"
hami_args=(
  --set devicePlugin.image.repository=hami --set devicePlugin.image.tag=local
  --set scheduler.extender.image.repository=hami --set scheduler.extender.image.tag=local
  --set devicePlugin.monitor.image.repository=hami --set devicePlugin.monitor.image.tag=local
  --set devicePlugin.nvidiaDriverRoot=/var/lib/nvml-mock/driver
  --set scheduler.kubeScheduler.image.tag="$K8S_VERSION")
"$ROOT/scripts/check-helm-render.sh" hami "$SRC_DIR/HAMi/charts/hami" "${hami_args[@]}"
helm upgrade --install hami "$SRC_DIR/HAMi/charts/hami" -n kube-system --kubeconfig "$kc" "${hami_args[@]}"

tk "$t" label node "$node" gpu=on --overwrite
tk "$t" -n kube-system set env daemonset/hami-device-plugin -c device-plugin DEVICE_DISCOVERY_STRATEGY=nvml
tk "$t" -n kube-system rollout restart daemonset/hami-device-plugin

# 不用 rollout status：vgpu-monitor 可能崩溃，以节点容量为准。
gpu_capacity_ok() {
  [[ "$(tk "$t" get node "$node" -o jsonpath='{.status.capacity.nvidia\.com/gpu}')" == "$expected_gpu" ]]
}
if ! wait_until 300 "$node nvidia.com/gpu=$expected_gpu" gpu_capacity_ok; then
  tk "$t" -n kube-system get pods -o wide
  tk "$t" describe node "$node" | grep -E 'nvidia.com/gpu|Taints' || true
  exit 1
fi
# device-plugin 只等主容器：vgpu-monitor 可能崩溃。
plugin_running() {
  [[ "$(tk "$t" -n kube-system get pods -l app.kubernetes.io/component=hami-device-plugin -o json \
    | jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | .status.containerStatuses[]? | select(.name == "device-plugin") | .ready] | any')" == true ]]
}
wait_until 180 "device-plugin 主容器 Ready" plugin_running
tk "$t" -n kube-system rollout status deploy/hami-scheduler --timeout=300s
# 确认用的是本地构建的镜像，避免 chart 键写错后静默用上游镜像。
tk "$t" -n kube-system get deploy/hami-scheduler ds/hami-device-plugin -o json \
  | jq -r '.items[].spec.template.spec.containers[] | "\(.name) \(.image)"'
if tk "$t" -n kube-system get deploy/hami-scheduler ds/hami-device-plugin -o json \
  | jq -e '[.items[].spec.template.spec.containers[].image | select(test("projecthami"))] | length > 0' >/dev/null; then
  die "HAMi 还在用上游镜像，不是 hami:local"
fi
tk "$t" -n kube-system get pods -o wide
tk "$t" describe node "$node" | grep nvidia.com/gpu
tk "$t" get node "$node" -o jsonpath='{.metadata.annotations.hami\.io/node-nvidia-register}{"\n"}'
# 这个 commit 启动时总会打印 mig 相关的 flag，所以只看策略是不是 none。
# 先存下日志再 grep：pipefail 下 grep -q 提前退出会让 kubectl 收到 SIGPIPE，整条管道判为失败。
echo "--- device-plugin 的 mig 策略"
mig_none() {
  local logs
  logs=$(tk "$t" -n kube-system logs daemonset/hami-device-plugin -c device-plugin) && grep -qE '"migStrategy": *"none"' <<<"$logs"
}
wait_until 60 "device-plugin 的 migStrategy 是 none" mig_none || die "device-plugin 的 migStrategy 不是 none"
tk "$t" -n kube-system logs daemonset/hami-device-plugin -c device-plugin | grep -F '"migStrategy"' || true
