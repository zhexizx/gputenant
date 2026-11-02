# 排障记录

记录真实遇到的报错。每条只写已经验证过的根因。

## 1. 从 WSL 访问租户 API 超时

- **现象：** TCP 已 Ready，`kubectl --kubeconfig tenant-a.kubeconfig get --raw /readyz` 报 `dial tcp 172.18.255.200:6443: i/o timeout`。
- **定位：** 同一地址从 `kamaji-control-plane` 容器里 curl 返回 `200`，说明控制面和 MetalLB 没问题。`docker info` 显示引擎是 `Docker Desktop`；WSL 里没有 `docker0` 或 `br-*` 网卡，`ip route get 172.18.255.200` 走默认网关。
- **根因：** kind 网桥在 Docker Desktop 的 VM 里，WSL 没有到 `172.18.0.0/16` 的路由。附录默认 Docker 是本机原生引擎。
- **修复：** 给每个租户起一个 socat 容器，把 `127.0.0.1:1644x` 转发到 LB IP；本地 kubeconfig 改用这个地址，并设置 `tls-server-name` 为 LB IP，证书校验仍然生效。工作节点用未改动的 `*.internal.kubeconfig`。

## 2. `kubeadm join` 等待 kubelet 超时

- **现象：** `The kubelet is not healthy after 4m0s`，`curl http://127.0.0.1:10248/healthz` 失败。
- **定位：** `journalctl -u kubelet` 报 `running with swap on is not supported`；节点内 `free -h` 显示 4 GiB swap，`/var/lib/kubelet/config.yaml` 里 `failSwapOn: true`。
- **根因：** 节点容器看到的是 Docker Desktop VM 的 swap。kind 节点能启动，靠的是 kind 生成配置时关闭了 `failSwapOn`；手动 join 的节点用的是 Kamaji 下发的默认配置。`--ignore-preflight-errors=all` 只跳过 kubeadm 的预检，管不到 kubelet。
- **修复：** 在 TCP 的 `kubelet.configurationJSONPatches` 里加 `failSwapOn: false`，删除节点容器后重新 join。

## 3. kube-proxy 和 Calico 同时崩溃，节点一直 NotReady

- **现象：** `kube-proxy` CrashLoopBackOff，`calico-node` 卡在 `Init:1/3`，节点报 `cni plugin not initialized`。
- **定位：** `install-cni` 日志是 `dial tcp 10.96.0.1:443: connection refused`，它是下游症状。`kube-proxy` 日志是 `open /proc/sys/net/netfilter/nf_conntrack_max: permission denied`。
- **根因：** Kamaji 生成的配置里 `conntrack.maxPerCore: null`，kube-proxy 用默认值算出 `32768 × 24 核 = 786432`，大于当前的 `262144`，于是尝试写入。节点容器里这个 sysctl 不可写，kube-proxy 退出 → ClusterIP 规则没有写入 → Calico 拿不到 token → CNI 未就绪。
- **第一次修复失败：** 把租户 `kube-proxy` ConfigMap 改成 `maxPerCore: 0`（kind 管理集群就是这个值）。触发 TCP reconcile 后，Kamaji 把它还原成 `null`，kube-proxy 再次按 `786432` 崩溃。`rollout status` 当时还报了成功，因为容器崩溃前短暂算作可用，所以不能只看它。
- **第二次修复失败：** 用 `--cpuset-cpus` 限制到 8 核，想让 `8 × 32768 = 262144` 等于当前值。节点内 `nproc` 确实是 8，kube-proxy 仍算出 `786432`。v1.36.4 的 `detectNumCPU` 用 cadvisor 读 `/sys` 的拓扑，节点内 `/sys/devices/system/cpu` 仍有 24 个 CPU，cpuset 影响不到。
- **没有采用：** 调高内核参数。从 Docker 能进入的网络命名空间（包括 `--pid=host` + `nsenter -t 1 -n`）看，这个 sysctl 都是只读的。
- **修复：** 不启用 TCP 的 `kubeProxy` addon，由 `manifests/kube-proxy.yaml` 自管，并设置 `--conntrack-max-per-core=0`。脚本改为检查 iptables 里的 `KUBE-SERVICES` 规则。
- **教训：** 改之前先确认资源归谁管；验证修复要看实际行为，不能只看 `rollout status` 或 `nproc`。

## 4. hami-scheduler CrashLoop，GPU Pod 静默退回默认调度器

- **现象：** TC-03 失败，`schedulerName` 是 `default-scheduler`；Pod 一直 Pending，事件是 `Insufficient nvidia.com/gpucores, nvidia.com/gpumem`；`hami-scheduler` 1/2 Ready。
- **定位：** `vgpu-scheduler-extender` 日志是 `unknown flag: --extender-bind`。它的镜像是 `projecthami/hami:v2.10.0`，不是 `hami:local`；`hami:local` 的 `--help` 里有这个 flag。
- **根因：** 附录的 `scheduler.image.*` 和 `scheduler.kubeScheduler.imageTag` 在这个 commit 的 chart 里不存在，Helm 不报错直接忽略。于是 HEAD 的 chart 配上了旧版镜像。正确的键是 `scheduler.extender.image.*`、`devicePlugin.monitor.image.*`、`scheduler.kubeScheduler.image.tag`。
- **附带观察：** webhook 不可用且 `failurePolicy=Ignore` 时，Pod 确实被放行并交给了默认调度器。这次是调度器崩溃引起的，不是隧道中断，不能当作 Day 4 的结论。
- **修复：** 改用正确的键，并在脚本里检查实际镜像，发现上游镜像就报错。

## 5. 实测和计划不一致的地方（不是报错）

- `nvidia.com/gpu.present` 标签：chart 0.6.0 不会自动打，它的 NOTES 要求手动打。脚本已补上。
- `grep -i mig` 不是“没有输出才正常”：这个 commit 启动时总会打印 mig 相关的 flag。实测 `migStrategy` 是 `none`，节点容量也正确。
- `vgpu-monitor`：换成 `hami:local` 后两个租户都是 2/2 Ready，没有复现计划里的 CrashLoopBackOff。
- 单卡显存：A100 是 40960 MiB，T4 是 16384 MiB，和计划的估算一致（Day 4 的 `over-card` 用 20000 合适）。

## 6. 预检把只读的 sysctl 判断成可写

- **现象：** 预检报 `可写=yes`，但 `nf_conntrack_max` 的权限是 `-r--r--r--`，conntrack 检查被跳过。
- **根因：** 特权容器里以 root 运行时，`[ -w 文件 ]` 不受权限位限制，总是成立。
- **修复：** 改为读 `stat -c %A` 的权限位。检查脚本自己也要验证：错误的 HAMi 键、停掉的调度器都做过反向测试，确认会失败。

## 预防措施

| 问题 | 检查 | 位置 |
|---|---|---|
| 1 网络 | 打印引擎类型和路由；确认代理端口没被占用 | `scripts/00-preflight.sh` |
| 2 swap | join 前确认有 swap 时已下发 `failSwapOn: false` | `scripts/03-tenant-worker.sh` |
| 3 kube-proxy | 预检 conntrack；发现 addon 就报错；触发 reconcile 和重启后再看 iptables 规则 | `00-preflight.sh`、`02-tenant-cp.sh`、`04-verify-env.sh` |
| 4 Helm 键 | 安装前渲染并检查镜像；部署后检查实际镜像 | `check-helm-render.sh`、`03-tenant-worker.sh`、`04-verify-env.sh` |
| 版本漂移 | Kamaji chart 存进仓库并校验 sha256，镜像固定 digest；记录运行中的镜像 | `01-management.sh`、`04-verify-env.sh` |
| 手工改动掩盖问题 | 一条命令删除、重建、跑全部测试 | `scripts/verify-from-scratch.sh` |

Docker Desktop 重启或升级后，先运行 `scripts/00-preflight.sh` 和 `scripts/04-verify-env.sh`。
