# kamaji-hami-lab 工作总结（2026-11-02）

依据 `多租户GPU平台-总计划.md` 附录的 Day 1–3，完成环境脚本和 TC-01~08 测试脚本。

## 当前状态

- 用最终版脚本删除环境、从零重建后，两个租户的 TC-01~08 全部通过，结果见 `results/20261102-164334/`。
- `up.sh`、`down.sh`、`verify-from-scratch.sh` 都实际运行过。
- 预防措施已加到脚本，总计划附录已更新。
- 还没有提交 git。

## 计划变更

总计划在 15:31 被修改：Day 2 从 mock-device-plugin 换成 nvml-mock 加本地构建的 HAMi。之后的工作按新版执行，没有使用附录末尾的备用方案。

## 产出

| 路径 | 内容 |
|---|---|
| `scripts/00-tools.sh` | 检查依赖，把 helm、jq 校验后装到 `.tools/bin` |
| `scripts/00-build-images.sh` | 按固定 commit 构建 `nvml-mock:local`、`hami:local` |
| `scripts/01-management.sh` | kind 管理集群、cert-manager、MetalLB、Kamaji |
| `scripts/02-tenant-cp.sh` | 租户控制面、导出 kubeconfig、API 代理 |
| `scripts/03-tenant-worker.sh` | 工作节点、kube-proxy、Calico、nvml-mock、HAMi |
| `scripts/00-preflight.sh` | 动手前的环境检查 |
| `scripts/check-helm-render.sh` | 安装前检查 Helm 渲染结果 |
| `scripts/04-verify-env.sh` | 触发 reconcile 和重启后，验证实际效果并记录版本 |
| `scripts/up.sh`、`scripts/down.sh`、`scripts/verify-from-scratch.sh` | 一键搭建、只删除本实验的资源、从零重建并跑全部测试 |
| `tests/tc-01.sh`~`tc-08.sh`、`tests/run-all.sh` | 8 项测试和汇总 |
| `manifests/kube-proxy.yaml` | 自管的 kube-proxy |
| `docs/troubleshooting.md` | 排障记录 |
| `results/` | 环境日志和每轮测试输出 |

## 固定的版本

| 组件 | 版本 |
|---|---|
| HAMi | commit `2b8f4e29c48bca376d4f6123d7c6ae7627aef579` |
| k8s-test-infra | commit `e7f2320c95bef01ddde12fb8c493019a558e8f43` |
| nvml-mock chart | 0.6.0 |
| Kubernetes（租户） | v1.36.4 |
| cert-manager / MetalLB / Calico | v1.21.2 / v0.16.1 / v3.31.2 |
| helm / jq | v3.19.0 / 1.8.2 |

Kamaji chart 存在 `manifests/charts/` 并校验 sha256，镜像固定为 `sha256:9f0cae2a…`。运行中的版本记录在 `results/env/versions.txt`。

## 测试结果

| TC | 内容 | 结论 |
|---|---|---|
| TC-01 | 控制面和 API 地址 | PASS |
| TC-02 | 节点和 Konnectivity 隧道 | PASS |
| TC-03 | webhook 把 GPU Pod 交给 `hami-scheduler` | PASS |
| TC-04 | 分配和绑定完成，节点锁自动释放 | PASS |
| TC-05 | 3 个 Pod 共享同一张卡 | PASS |
| TC-06 | 算力超分被拒绝（`CardInsufficientCore`） | PASS |
| TC-07 | 显存超分被拒绝（`CardInsufficientMemory`） | PASS |
| TC-08 | 租户隔离，包括跨租户凭证被拒绝 | PASS |

计划没有逐项定义 TC-03、04、05，上表的划分是实现时定的。

各轮记录：

| 运行 | 环境 | 结果 |
|---|---|---|
| `results/20261102-160456/` | tenant-a，调度器有故障 | TC-01、02 通过，TC-03 失败，TC-04 运行中被手动停止 |
| `results/20261102-161625/` | 修复后的 tenant-a | TC-01~07 通过 |
| `results/20261102-161915/` | 两个租户 | TC-01~08 通过 |
| `results/20261102-162928/` | 从零重建 | TC-01~08 通过 |
| `results/20261102-164334/` | 加入预防措施后从零重建 | TC-01~08 通过 |

## 解决的问题

详情见 `docs/troubleshooting.md`。

1. **WSL 访问不到租户 API：** kind 网桥在 Docker Desktop 的 VM 里，WSL 没有路由。加了 socat 代理。
2. **kubelet 起不来：** 节点开了 swap。在 TCP 里设置 `failSwapOn: false`。
3. **kube-proxy 崩溃，连带 Calico 失败：** 它要写只读的 conntrack sysctl。改 ConfigMap 被 Kamaji 还原，限制 cpuset 也无效，最后关掉 addon，自己部署 kube-proxy。
4. **`hami-scheduler` 崩溃：** 附录的 Helm 键不存在且被静默忽略，用上了不匹配的上游镜像。改用正确的键，脚本会检查实际镜像。

## 和计划不一致的地方

- 没有启用 Kamaji 的 `kubeProxy` addon。
- HAMi 镜像参数改为 `scheduler.extender.image.*`、`devicePlugin.monitor.image.*`、`scheduler.kubeScheduler.image.tag`。
- `nvidia.com/gpu.present` 标签要手动打。
- `vgpu-monitor` 是 2/2 Ready，没有复现计划说的崩溃。
- `grep -i mig` 会有输出，实际 `migStrategy` 是 `none`。
- 实测单卡显存：A100 40960 MiB，T4 16384 MiB。

## 局限

- nvml-mock 的驱动是假的，这些测试不能证明运行时的显存和算力限制，也没有性能数据。
- 调度器崩溃时，Pod 因 `failurePolicy=Ignore` 退回了默认调度器。起因不是隧道中断，不能当作 Day 4 的结论。

## 下一步

1. Day 4 降级实验。开始前先运行 `scripts/04-verify-env.sh tenant-b`。
