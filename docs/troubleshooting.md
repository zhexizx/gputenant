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

## 7. 在节点 FORWARD 链上加 DROP，隧道断不掉

- **现象：** Day 4 第一轮（`results/day4-5-20261102-173829/baseline/`）按计划在 `tenant-b-worker` 的 `OUTPUT` 和 `FORWARD` 链插入 `-d <LB_IP> --dport 8132 -j DROP`。之后 4 分钟里 `kubectl logs ds/konnectivity-agent` 一直成功，门禁没有通过，没有创建测试 Pod。结束时 FORWARD 的 DROP 只命中了 13 个包。
- **定位：**
  - agent 不是 hostNetwork（Pod IP `10.244.66.130`），到 LB 的流量走节点的 FORWARD 链，不走 OUTPUT。
  - 单独复现，每 10 秒看一次规则顺序：刚插入时 DROP 排在第 1 位，10 秒内 `cali-FORWARD` 就回到第 1 位，DROP 被挤到第 2 位。
  - agent 对应的 `cali-fw-calid395ae9544c` 第一条规则是 `--ctstate RELATED,ESTABLISHED -j ACCEPT`。
  - 复现时隧道断了，并且一直没恢复，DROP 的计数持续增加。
- **根因：** Felix 会定期把自己的跳转规则插回链的最前面。之后 agent 已经建立的连接在 `cali-fw-*` 里就被 ACCEPT 了，到不了 DROP，只有新连接会被拦下。隧道断不断，要看 DROP 还排第一的那几秒里旧连接有没有断：断了，重连就会一直被拦，复现时就是这样；没断，隧道就一直通，第一轮就是这样。所以这种阻断方式结果不稳定。
- **修复：** 规则改为通过 `nsenter -t <agent PID> -n` 加到 agent Pod 网络命名空间的 OUTPUT 链。这个命名空间里没有 Calico 的规则，已有连接也会被拦下。阻断期间每次检查都确认 agent PID 没变、规则还在：Pod 重建会换一个命名空间，规则就丢了，这时整轮作废。见 `experiments/lib.sh`。
- **教训：** 规则加上了不等于生效。要看规则的顺序、计数，还有实际的连接状态。

## 8. 准入策略已生效，门禁却一直判定没生效

- **现象：** `results/day4-5-20261102-193852/fix-b/` 里，apply 准入策略后等了 90 秒，门禁报 `准入策略没有生效`，`fix-b` 中止。
- **定位：** 手动 apply 同一个策略，15 秒后 `status.typeChecking` 为空（没有类型错误）。用 dry-run 创建门禁 Pod 返回 `denied request: GPU Pod must use hami-scheduler`，说明策略已经生效。
- **根因：** 门禁写成了 `kubectl create --dry-run=server ... | grep -q '<拒绝信息>'`。`scripts/lib.sh` 开了 `set -euo pipefail`，后面的 `set +e` 只关掉了 `-e`，`pipefail` 还在。请求被拒绝时 kubectl 返回 1，即使 grep 匹配到了，整条管道也返回 1。
- **修复：** 先保存 kubectl 的输出，再判断里面有没有拒绝信息。其他管道里前一个命令都是正常返回，不受影响。
- **教训：** 要检查失败输出时，不要把会返回非零的命令接到 `grep -q` 前面。

## 9. 实验前置检查把自己的启动命令当成冲突

- **现象：** `verify-from-scratch.sh` 和 `experiments/run.sh` 写在同一行命令里，重建通过后，实验立刻报 `有其他测试或环境脚本在运行`，打印出来的是这行命令本身（`results/day4-5-20261103-111733/`）。
- **根因：** `guard` 用 `pgrep -af` 按命令行匹配 `scripts/verify-from-scratch.sh` 等脚本名。整行命令的父 shell 一直在运行，命令行里包含这个脚本名，于是匹配到了自己。
- **修复：** `guard` 匹配时排除自己和全部祖先进程（沿 `/proc/<pid>/status` 的 `PPid` 往上找）。保护没有放宽：真正在运行的环境脚本是独立的进程，不会是实验的祖先，照样会被拦下。已用两个场景验证：外层命令行带着已结束的 `scripts/verify-from-scratch.sh` 时不再误报；另起一个 `scripts/up.sh` 进程时仍然报冲突。2026-11-09 在真实实验里复现了原场景：`bash -c './scripts/04-verify-env.sh tenant-a; ./experiments/run.sh'`，前一个脚本已结束、父 shell 的命令行里还带着它，前置检查判定没有冲突，基线、方案 A、方案 B、webhook 故障四步都返回 0（`results/day4-5-20261109-162805/`）。

## 10. 上一步的隧道告警还没解除，webhook 故障实验就中止了

- **现象：** `results/day4-5-20261103-111859/` 前三步通过，`webhook-down.sh` 一开始就报 `开始前就有告警在 firing`。
- **根因：** 方案 B 刚断过隧道，恢复后 `ApiserverToNodePathDown` 和 `KonnectivityAgentDisconnected` 要过一两分钟才解除；前置检查只查一次。
- **修复：** 改为最多等 300 秒，等故障状态告警全部解除再开始。

## 11. 事件型告警被判为“这次故障触发的”，其实是上一步留下的

- **现象：** `results/day4-5-20261103-113034/webhook-down/` 里 `CHECK PASS: HamiWebhookFailOpen 真实触发`，但它的 `activeAt` 比缩容早 149 秒。
- **根因：** `HamiWebhookFailOpen` 看最近 5 分钟的放行次数。上一步方案 B 的隧道实验里也有放行，这条告警在 webhook 实验开始时还在 firing。检查只看“观察期间是不是 firing”，没看它是什么时候开始的。另外，告警的“首次观察到”时间是在探针跑完后才开始记录的，只是上限，不是触发时间。
- **修复：** 开始前等它解除（最多 420 秒）；只有 `activeAt` 晚于缩容才算通过；报告里同时写 `activeAt`。重跑结果见 `results/webhook-down-20261103-115651/`。
- **教训：** 判断“是这次故障引起的”，要先确认故障前它不在，再看它是什么时候开始的。

## 12. WSL 里拉取 ghcr.io 的 chart 失败

- **现象：** 从零重建时，nvml-mock chart 渲染失败，报 `failed to perform "FetchReference" ... ghcr.io ... exit status 1`，前面有一行 `WSL ... UtilAcceptVsock:271: accept4 failed 110`。
- **定位：** 几分钟后手动 `helm show chart` 同一个 chart，第一次就成功；重跑整个重建也通过了。
- **根因（推测，没有验证）：** Helm 访问 OCI 仓库时会调用 Docker Desktop 的凭证程序，这要经过 WSL 和 Windows 之间的 vsock，那次连接超时了。
- **处理：** 重跑。如果以后经常出现，再给 chart 拉取加重试，或者像 Kamaji 一样把 chart 存进仓库。

## 13. 容器化的 API 在 WSL 里访问不到

- **现象：** `gputenant` 镜像以 `--network host` 运行 `serve`，容器内 `wget 127.0.0.1:18080/healthz` 返回 ok，WSL 里 `curl 127.0.0.1:18080` 连接被拒（rc=7）。第一次用 8081 端口时更隐蔽：curl 拿到 `404 page not found`，因为 WSL 里另一个进程（ModelServe 的 operator）占着 8081，请求根本没到容器。
- **定位：** 改成监听 `0.0.0.0` 后，WSL 和 `host.docker.internal` 仍然不通，但 kind 网络里的容器经网关 `172.18.0.1:18080` 能访问。说明 Docker Desktop 的“宿主机网络”是它虚拟机自己的网络，不是 WSL 的。之前容器里能访问管理集群的 `127.0.0.1:<端口>`，是因为 kind 的端口映射也在这个虚拟机里。
- **根因：** 和租户 API 一样（第 1 条）：WSL 到不了 Docker Desktop 虚拟机里的网络，只有 `-p` 发布的端口会转发过来。
- **修复：** 和租户 API 的做法一致，加一个 kind 网络里的 socat 容器，`-p 127.0.0.1:18081` 转到 `172.18.0.1:18080`（`deploy/docker-compose.yml` 的 `api-proxy`）。发布端口不能和宿主机网络里 API 的端口相同，否则报 `address already in use`。Prometheus 在 kind 网络里，直接抓 `172.18.0.1:18080`。原生 Linux 上宿主机网络就是本机，不需要这一层。
- **教训：** 测试脚本拿到响应时要确认是谁回的：`404 page not found` 不是本服务的报错格式，应该马上怀疑端口被别人占了。

## 14. GitHub 对 Prometheus chart 间歇返回 504

- **现象：** 2026-11-09 跑 `experiments/alert.sh`，`helm upgrade` 报 `failed to fetch .../prometheus-29.40.3.tgz : 504 Gateway Timeout`，实验中止（`results/day4-5-20261109-162805/`、`results/day4-5-20261109-164827/`）。之后一轮实验本身通过，但结束时恢复 Prometheus 默认配置又遇到 504，环境停在叠加了实验规则的状态（`results/day4-5-20261109-165724/`）。
- **定位：** `curl -I` 同一个地址，GitHub 在 0.06 秒内直接返回 504（`x-github-edge-region: australiaeast`）；同一时间别的版本的 release 文件（`prometheus-29.39.0.tgz`）返回正常的 302。16:47 到 17:10 之间时好时坏：每 30 秒探测一次，约 8 分钟后恢复，几分钟后又出现。
- **根因：** GitHub 边缘节点对这一个 release 文件的问题，不在本项目。本项目受影响是因为 Prometheus chart 每次都从 GitHub 下载，没有用本地缓存。
- **处理：** 等这个文件能下载后重跑 `alert.sh`，再用 `experiments/restore.sh` 恢复（重试第 3 次成功，实验规则已移除，两个租户 `status` 都是 14/14）。没有改代码。`gputenant create` 的 `monitoring` 步骤也会遇到同样的问题，重跑即可，已完成的步骤会跳过。如果以后经常出现，像 Kamaji 一样把 chart 存进仓库并校验 sha256。
- **教训：** 恢复步骤也依赖网络时，恢复本身可能失败。脚本这次正确地报了“环境恢复失败”，没有假装恢复成功，所以要看最后一行再离开。

## 预防措施

| 问题 | 检查 | 位置 |
|---|---|---|
| 1 网络 | 打印引擎类型和路由；确认代理端口没被占用 | `scripts/00-preflight.sh` |
| 2 swap | join 前确认有 swap 时已下发 `failSwapOn: false` | `scripts/03-tenant-worker.sh` |
| 3 kube-proxy | 预检 conntrack；发现 addon 就报错；触发 reconcile 和重启后再看 iptables 规则 | `00-preflight.sh`、`02-tenant-cp.sh`、`04-verify-env.sh` |
| 4 Helm 键 | 安装前渲染并检查镜像；部署后检查实际镜像 | `check-helm-render.sh`、`03-tenant-worker.sh`、`04-verify-env.sh` |
| 版本漂移 | Kamaji chart 存进仓库并校验 sha256，镜像固定 digest；记录运行中的镜像 | `01-management.sh`、`04-verify-env.sh` |
| 7 隧道阻断 | 阻断规则加在 agent 命名空间；API 正常且隧道连续 3 次不通，才创建测试 Pod；阻断期间检查 agent PID 和规则 | `experiments/lib.sh` |
| 9 实验冲突 | 实验单独运行，不和环境脚本写在同一行 | `experiments/lib.sh` 的 `guard` |
| 10、11 告警残留 | 开始前等告警解除；按 `activeAt` 判断是不是这次触发的 | `experiments/webhook-down.sh` |
| 默认防护缺失 | 每个租户默认部署准入策略和告警；验证环境和 TC-09 都会检查 | `05-tenant-guardrails.sh`、`04-verify-env.sh`、`tests/tc-09.sh` |
| 手工改动掩盖问题 | 一条命令删除、重建、跑全部测试 | `scripts/verify-from-scratch.sh` |

Docker Desktop 重启或升级后，先运行 `scripts/00-preflight.sh` 和 `scripts/04-verify-env.sh`。
