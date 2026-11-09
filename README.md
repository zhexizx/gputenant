# gputenant

基于 Kamaji 托管控制面和 HAMi GPU 虚拟化的多租户 GPU Kubernetes 平台，跑在 kind 上，GPU 由 nvml-mock 模拟。

- 每个租户有独立的 Kamaji 控制面和独立的工作节点（`kindest/node` 容器）。
- HAMi 按显存和算力切分 GPU，同一租户内的多个 Pod 可以共享一张卡，不能跨租户共享。
- 核心实验：Konnectivity 隧道断开或 HAMi webhook 挂掉时，GPU Pod 会被静默放行、绕过 HAMi 核算；用 ValidatingAdmissionPolicy 改为明确拒绝，并用 Prometheus 告警发现故障。

总计划原文在 `多租户GPU平台-总计划.md`（仓库外）。它的附录已过时，**以本仓库为准**。

## 当前状态（2026-11-09）

| 部分 | 状态 | 证据 |
|---|---|---|
| 环境：管理集群、两个租户、nvml-mock、HAMi | ✅ 可以从零重建，租户由 CLI 开通 | `scripts/up.sh`、`scripts/verify-from-scratch.sh` |
| 功能测试 TC-00~09 | ✅ 用 CLI 从零重建后全部通过 | `results/20261108-172558/` |
| 降级实验：隧道断开、webhook 挂掉、方案 A/B | ✅ 完成 | `docs/failure-report.md`、`results/day4-5-20261103-113034/`、`results/webhook-down-20261103-115651/` |
| 默认防护：准入策略和 4 条告警，新租户自动部署 | ✅ 完成，告警都真实触发过 | `scripts/05-tenant-guardrails.sh`、TC-09 |
| `gputenant create`、`delete` | ✅ 14 个 Step；kill 后重跑能接着做完，半成品租户能删干净 | `results/cli-20261108-165831/`、`docs/work-summary-2026-11-08.md` |
| `gputenant status`、`verify` | ✅ `status` 约 3 秒返回，有告警 firing 时判为 degraded；`verify` 在 hami-scheduler 停掉时报失败，恢复后通过 | `results/cli-status-verify-20261108-204408/` |
| 本地 e2e、PR CI | ✅ `scripts/e2e.sh` 通过；PR CI（gofmt、vet、单元测试、构建、shellcheck）在 GitHub 上通过 | `results/e2e-20261108-210436/`、PR #8 |
| REST API（`gputenant serve`）和 `/metrics` | ✅ 202/409/404；API 被 `kill -9` 后重启，旧任务 404，半成品租户报 not ready，指标 `tenant_ready=0`；再次提交从断点续做 | `results/api-20261109-095837/` |
| `gputenant` 镜像和 GHCR 发布 | ✅ 本地构建的镜像从零开通、删除租户都通过，文件属主是当前用户；打 tag `v0.1.0` 后 release workflow 发布了 `gputenant:v0.1.0`，以及按固定 commit 构建的 `hami`、`nvml-mock` | `results/image-20261109-100723/`、release run 42546815733 |
| CI e2e | ✅ GitHub Actions 上通过（约 9 分钟）：从 GHCR 拉镜像、建管理集群、开通 tenant-a、verify、TC-00~07 和 TC-09 | e2e run 42545273247（第 2 次运行；第 1 次在镜像发布前触发，拉镜像失败） |
| Grafana 看板 | ✅ 15 个面板的查询逐个在真实数据上验证过，包括故障时的降级和告警；面板外观还要人工在浏览器里看 | `results/dashboard-20261109-101717/`、`deploy/` |
| 真实 GPU 验证 | ⬜ 有条件执行，见下文 | |

**还没收尾的：**

- `experiments/lib.sh` 的 `guard` 改成了排除自己的祖先进程（`docs/troubleshooting.md` 第 9 条）。这个改动只用模拟场景测过，还没在真实实验里跑过。

## 运行

```bash
./scripts/up.sh                  # 搭建环境
./tests/run-all.sh               # TC-00~09
./scripts/e2e.sh                 # CLI 开通（已完成的步骤跳过）→ gputenant verify → TC-00~09
./experiments/run.sh             # 降级实验：基线 → 方案 A → 方案 B → webhook 故障 → 告警
./experiments/restore.sh         # 实验被强行中断后恢复
./scripts/verify-from-scratch.sh # 删除、从零重建、跑全部测试
./scripts/dashboard.sh up        # API + Prometheus + Grafana（先 docker build -t gputenant:dev .）
./scripts/down.sh                # 只删除本实验的资源
```

`up.sh` 会构建 `bin/gputenant` 并用它开通租户。手动使用 CLI 前先 `source scripts/lib.sh`（设置 `KUBECONFIG` 和 `GPUTENANT_HOME`）：

```bash
bin/gputenant create tenant-c --gpu-profile t4 --gpu-count 2  # 可重跑，已完成的步骤跳过
bin/gputenant status tenant-c   # 只读：每一步的状态、GPU 容量、firing 的告警
bin/gputenant verify tenant-c   # 冒烟测试，会重启 kube-proxy、跑一个短暂的 GPU Pod
bin/gputenant delete tenant-c
```

REST API（`bin/gputenant serve` 默认只监听 `127.0.0.1:8080`：它能开通和删除租户）：

```bash
curl -X POST localhost:8080/tenants -d '{"name":"tenant-c","gpuProfile":"t4","gpuCount":1}'  # 202 + 任务
curl localhost:8080/tasks/<任务 ID>     # 进度；API 重启后旧任务返回 404
curl localhost:8080/tenants/tenant-c    # 每次重新跑 Check，状态以集群为准
curl -X DELETE localhost:8080/tenants/tenant-c
curl localhost:8080/metrics
```

镜像 `gputenant`（CLI 和 API）要用宿主机网络、挂载 `docker.sock`，并以当前用户运行，否则写出来的 kubeconfig 属于 root。`scripts/dashboard.sh up` 已经处理好这些。WSL + Docker Desktop 上本机访问不到宿主机网络里的端口，API 经 socat 代理暴露在 `127.0.0.1:18081`（`docs/troubleshooting.md` 第 13 条）。

租户 kubeconfig 在 `kubeconfigs/<租户>/`（脚本把 `GPUTENANT_HOME` 默认设为 `kubeconfigs/`；单独运行 CLI 时默认是 `~/.gputenant/`），含集群管理员凭证，已被 `.gitignore` 排除。排障记录见 `docs/troubleshooting.md`。

## 剩余计划

恢复时按顺序做，每块大约 1 天。原计划是 11/3–11/7，因为暂停，日期要重新排。operator 的 7 天和面试准备的时间不能被这个项目占用。

| 顺序 | 任务 | 完成标准 |
|---|---|---|
| 0 | 推送并合并 `feat/default-guardrails`；GitHub 仓库改名为 `gputenant`；建好 Go 项目骨架 | 分支已合并；`go build` 通过 |
| 1 | `gputenant create`、`delete` | 用 CLI 从零建好两个租户，TC-00~09 通过；中途 kill 掉再重跑能成功；只建了一半的租户能删干净 |
| 2 | `status`、`verify`、本地 e2e、PR CI | PR CI 跑通；本地 e2e 通过 |
| 3 | Gin API、GHCR 发布、CI e2e、Grafana 看板 | API 重启后不会误报成功；CI e2e 跑通；看板可以演示 |
| 4 | README 和 `docs/` 定稿；有时间的话做真实 GPU 验证 | 文档齐全 |

第 1 块没跑通之前，Grafana 和 GHCR 往后放。CI e2e 从第 2 块挪到第 3 块：它要从 GHCR 拉镜像，而且测试目前假设 tenant-a 是 a100，要先改成能在单个 t4×2 租户上跑。

## 已定的设计

**代码组织**

- Go 代码放在本仓库：`cmd/gputenant`、`internal/tenant`。CLI 和 API 共用 `internal/tenant`。
- 现有的 bash 开通脚本（`scripts/02`、`03`、`05`）留作参照，也用来数手工步骤数。CLI 完成后，`up.sh` 改为调用 CLI 开通租户。
- e2e 不重写成 Go：先用 CLI 开通租户，再跑现有的 `tests/tc-*.sh`。`experiments/` 继续在 CLI 建好的租户上运行。

**CLI**

- 只管租户的 `create`、`delete`、`status`、`verify`。管理集群（`01-management.sh`）和镜像构建（`00-build-images.sh`）留在 bash 里，CLI 的前置检查只确认它们已就绪。
- 开通流程的每一步实现 `Check`/`Apply`：重跑时跳过已完成的步骤，进度全靠查集群和 Docker 判断，不存状态文件。`delete` 按相反顺序执行，每一步都容忍资源不存在。
- `create` 只开放 `--gpu-profile`（a100、t4）和 `--gpu-count`。准入策略和告警总是部署，没有关闭开关；实验需要去掉时，由 `experiments/` 的脚本临时删除。
- API 代理端口从 16443 开始，自动找第一个没被 `*-api-proxy` 容器占用的；LB IP 从 Service 读取。
- kubeconfig 写到 `~/.gputenant/<租户>/`。测试通过环境变量 `GPUTENANT_HOME` 找到它，默认还是 `kubeconfigs/`。
- `status` 只读、几秒内返回：每一步的 `Check` 结果、节点的 `nvidia.com/gpu` 容量、正在 firing 的告警。步骤没完成或有状态告警 firing 时退出码非 0。期望的 GPU 数从租户里 nvml-mock 的 Helm release 读回，所以 `status`、`verify` 不需要 `--gpu-*` 参数；`create` 的参数和已装的不一致时直接报错。
- `verify` 是冒烟测试：相当于 `04-verify-env.sh`，再加两项：一个申请 `gpumem` 的 Pod 由 `hami-scheduler` 调度并 Running；用 dry-run 确认跳过 webhook 的 GPU Pod 被准入策略拒绝。完整的 TC 交给 e2e。
- Kubernetes 操作用 `client-go` 和 dynamic client；Docker 和 Helm 调用命令行，封装在接口后面。

**CI 和发布**

- 每个 PR 跑 lint、单元测试和构建；合入 `main`、定时或手动触发时跑 e2e。
- CI 的 e2e 只建 tenant-a（a100×1，和本地同一个租户），跑 TC-00~07 和 TC-09。原定 t4×2，但 TC-04~06 要 3 个 10000 MiB 的 Pod 共享一张卡，t4 每张卡只有 16384 MiB，用同一个租户就不用给测试写两套参数。TC-08（租户隔离）需要两个租户，受 runner 资源限制，只在本地跑。
- 打版本 tag 时发布到 GHCR：`gputenant` 镜像（CLI 和 API，内含 docker CLI 和 helm，运行时挂载 `docker.sock`），以及按固定 commit 打 tag 的 `hami`、`nvml-mock` 镜像，供 CI 拉取，CI 里不从源码构建。

**API 和看板**

- 接口按总计划：`POST`/`DELETE` 返回 `202` 和任务 ID；同一租户同时只能有一个任务，重复请求返回 `409`；只运行一个实例，任务进度放在内存里，重启后以 `Check` 查到的实际状态为准。
- API 的 `/metrics` 导出跨租户的指标：租户状态、开通耗时和结果、GPU 容量和已分配量（从 HAMi 注解计算）、各租户 firing 的告警。租户状态由后台每 30 秒刷新成快照，抓取时直接读快照；读不到告警时不导出告警数，免得被当成 0。
- 告警不经 apiserver 代理查询（原计划如此），和 CLI 一样在节点内 curl 租户 Prometheus 的 ClusterIP：apiserver 代理要经过 Konnectivity 隧道，隧道断开时正好查不到。
- 另起一个 Prometheus 和一个 Grafana，用 docker compose 跑在 kind 网络上，只抓取 API 的 `/metrics`（`deploy/`）。看板定义存进仓库。看板显示的是**分配率**，不是利用率。

**简历数字**

- `N`：开通流程的 Step 数量，不数单条命令。**实际是 14 个**（`internal/tenant/plan.go`）。
- `X`：管理集群已就绪、镜像已构建时，3 次 `time gputenant create` 的中位数，记录在 `results/`。**实测 214.5 秒（约 3 分 34 秒）**：3 轮分别是 214.3、214.5、221.7 秒，都是从零开通 t4×2 的 tenant-c，结束后 `status` 14/14（`results/measure-x-20261109-140325/`，`scripts/measure-create.sh`）。
  - 测量条件：WSL2 + Docker Desktop（24 CPU、16 GB），同时运行着两个租户和另一个 kind 集群；Calico manifest 已有本地缓存，Prometheus chart 每次从网上下载。唯一跳过的步骤是共享的 `tenants` 命名空间（已存在）。
  - 最耗时的三步：monitoring（Prometheus，约 60 秒）、calico（约 55 秒）、images（导入本地镜像，约 30 秒）。
  - 写简历时写“约 3.5 分钟”，并注明 GPU 是模拟的、单机 kind 环境。

## 真实 GPU 验证（有条件执行）

- 只在其他部分全部完成并验证后才做；否则顺延到投递以后。
- 先用借记卡注册 DigitalOcean，确认能绑卡、控制台能看到 GPU 机型；绑不上就顺延。Lambda 不收借记卡。AWS、GCP 的 GPU 配额要审批，赶不上。
- 预算上限 30 美元，用 k3s 单节点，按总计划做 3 项验证（显存可见范围、显存硬限制、算力限制），用完立刻销毁实例。

## 局限

- **nvml-mock 只覆盖调度和核算这一层。** HAMi 的 device-plugin、调度器、webhook 都是真实运行的，所以分配、共享、超分拒绝和故障行为都是真实结果。容器内的运行时限制（HAMi-core 拦截 CUDA 调用，限制显存和算力）没有验证：驱动是假的，测试 Pod 里还设了 `CUDA_DISABLE_CONTROL=true`。没做真实 GPU 验证之前，不能说运行时隔离有效。
- 同一张卡只能被同一租户内的 Pod 共享，不能跨租户共享。
- 把 `kindest/node` 容器接入为工作节点只是实验做法，不是生产方案。
- 没测的故障：隧道“黑洞”（server 以为 agent 在线，但数据不通，webhook 可能等满 10 秒超时）；方案 A 对 UPDATE 操作的影响。方案 A 已经不采用。
