#!/usr/bin/env bash
# 动手前的环境检查，几秒内结束。发现会导致失败的问题就退出。
source "$(dirname "$0")/lib.sh"
errors=0
ok()  { echo "ok:   $*"; }
bad() { echo "FAIL: $*"; errors=$((errors + 1)); }
info() { echo "info: $*"; }

for c in docker kind kubectl helm jq curl git; do
  command -v "$c" >/dev/null && ok "$c" || bad "缺少 $c，先运行 00-tools.sh"
done
docker info >/dev/null 2>&1 || { bad "Docker 引擎不可用"; exit 1; }

os=$(docker info --format '{{.OperatingSystem}}')
ncpu=$(docker info --format '{{.NCPU}}')
mem_gib=$(($(docker info --format '{{.MemTotal}}') / 1024 / 1024 / 1024))
info "Docker 引擎: $os，$ncpu CPU，${mem_gib} GiB"
((mem_gib >= 8)) && ok "内存足够（两个租户实测约 5 GiB）" || bad "内存少于 8 GiB"

# 问题 1：宿主机能否直达 kind 网段，决定了为什么要走 socat 代理。
if docker network inspect kind >/dev/null 2>&1; then
  subnet=$(docker network inspect kind | jq -r '.[0].IPAM.Config[].Subnet | select(test("^[0-9.]+/"))' | head -1)
  gw=$(docker network inspect kind | jq -r '.[0].IPAM.Config[] | select(.Subnet | test("^[0-9.]+/")) | .Gateway')
  if ip -br addr 2>/dev/null | grep -qF " $gw/"; then
    info "kind 网桥 $subnet 在本机，LB IP 也可直连；脚本仍统一走代理"
  else
    info "本机没有 kind 网桥 $subnet，LB IP 不可直连，kubectl 走 127.0.0.1 上的 socat 代理"
  fi
fi

# 代理端口要么空闲，要么就是我们自己的代理。
for t in tenant-a tenant-b; do
  port=$(tenant_api_port "$t")
  owner=$(docker ps --filter "publish=$port" --format '{{.Names}}')
  if [[ "$owner" == "$t-api-proxy" ]]; then
    ok "端口 $port 由 $owner 使用"
  elif [[ -n "$owner" ]]; then
    bad "端口 $port 被容器 $owner 占用"
  elif (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    bad "端口 $port 被其他进程占用"
  else
    ok "端口 $port 空闲"
  fi
done

# 问题 2、3：从节点容器的视角读取，和实际运行环境一致。
read -r swap_kb conntrack_max conntrack_writable < <(docker run --rm --privileged "$PREFLIGHT_IMAGE" sh -c \
  'awk "/SwapTotal/ {printf \"%s \", \$2}" /proc/meminfo
   printf "%s " "$(cat /proc/sys/net/netfilter/nf_conntrack_max 2>/dev/null || echo 0)"
   # root 下 [ -w ] 总是成立，要看权限位。
   case "$(stat -c %A /proc/sys/net/netfilter/nf_conntrack_max 2>/dev/null)" in -rw*) echo yes ;; *) echo no ;; esac')
info "swap=${swap_kb}kB，nf_conntrack_max=$conntrack_max，可写=$conntrack_writable"
if ((swap_kb > 0)); then
  if grep -q 'path: /failSwapOn' "$ROOT/scripts/02-tenant-cp.sh"; then
    ok "有 swap，TCP 已设置 failSwapOn: false"
  else
    bad "有 swap，但 02-tenant-cp.sh 没有设置 failSwapOn: false"
  fi
fi
wanted=$((32768 * ncpu))
if ((wanted > conntrack_max)) && [[ "$conntrack_writable" == no ]]; then
  if grep -q -- '--conntrack-max-per-core=0' "$ROOT/manifests/kube-proxy.yaml"; then
    ok "kube-proxy 默认要写 conntrack=$wanted 但只读，自管 kube-proxy 已设置 --conntrack-max-per-core=0"
  else
    bad "kube-proxy 会因写 nf_conntrack_max 失败而崩溃，见 troubleshooting 第 3 条"
  fi
fi

((errors == 0)) || die "预检发现 $errors 个问题"
echo "预检通过"
