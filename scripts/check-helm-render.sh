#!/usr/bin/env bash
# Helm 会静默忽略不存在的键，所以安装前渲染一次，确认设置真的生效。
# 用法: check-helm-render.sh <hami|nvml-mock> <chart> [helm 参数...]
source "$(dirname "$0")/lib.sh"
kind_="$1"; chart="$2"; shift 2
rendered=$(helm template "$kind_" "$chart" -n kube-system "$@") || die "$kind_ 渲染失败"
images=$(grep -E '^\s*-?\s*image:' <<<"$rendered" | sed -E 's/.*image:\s*//; s/"//g' | sort -u)
echo "$kind_ 渲染出的镜像:"; sed 's/^/  /' <<<"$images"

case "$kind_" in
  hami)
    # 只看 Deployment 和 DaemonSet 的工作负载；证书 Job 用上游 certgen 是正常的。
    bad=$(grep -E 'projecthami/' <<<"$images" || true)
    [[ -z "$bad" ]] || die "仍在使用上游 HAMi 镜像，可能是键名写错: $bad"
    grep -q 'hami:local' <<<"$images" || die "渲染结果里没有 hami:local"
    grep -qF "kube-scheduler:$K8S_VERSION" <<<"$images" || die "kube-scheduler 版本不是 $K8S_VERSION"
    grep -q '/var/lib/nvml-mock/driver' <<<"$rendered" || die "nvidiaDriverRoot 没有生效"
    ;;
  nvml-mock)
    grep -q 'nvml-mock:local' <<<"$images" || die "渲染结果里没有 nvml-mock:local"
    ;;
  *) die "未知的 chart 类型: $kind_" ;;
esac
echo "$kind_ 渲染检查通过"
