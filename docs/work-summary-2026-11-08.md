# gputenant 工作总结（2026-11-08）

按 README「剩余计划」完成第 0 块和第 1 块（`gputenant create`、`delete`，PR #7），第 2 块（`status`、`verify`、本地 e2e、PR CI）代码写完并在真实环境验证，分支 `feat/cli-status-verify`。

## 第 0 块：已完成

| 任务 | 证据 |
|---|---|
| 合并 `feat/default-guardrails` | PR #5 |
| GitHub 仓库改名为 `gputenant` | `git remote -v` 指向 `zhexizx/gputenant.git`，`git fetch` 成功 |
| Go 项目骨架，`go build` 通过 | PR #6（`7c14aca`，合并提交 `fe55a18`） |

## 第 1 块：`create`、`delete`

### 完成标准对照

| 完成标准 | 结果 | 证据 |
|---|---|---|
| 用 CLI 从零建好两个租户 | ✅ tenant-a（a100×1，`nvidia.com/gpu=10`）、tenant-b（t4×2，`=20`），代理端口自动分到 16443、16444 | `results/cli-20261108-165831/create-a-*.log`、`create-b.log` |
| TC-00~09 通过 | ✅ 10 个 TC 全部 PASS；`04-verify-env.sh` 两个租户都通过 | `results/20261108-170801/`（代码版本 `fe55a18-dirty`：改动还没提交）、`results/cli-20261108-165831/verify-env.log` |
| 中途 kill 掉再重跑能成功 | ✅ 在 calico 步骤发 SIGINT，进程退出；重跑时前 8 步 skipped，从 calico 接着做完 | `create-a-1-killed.log`、`create-a-2-rerun.log` |
| 已建好的租户重跑不做任何事 | ✅ 14 步全部 skipped，2 秒返回 | `create-a-3-idempotent.log` |
| 只建了一半的租户能删干净 | ✅ tenant-c 在 join 完成后 kill，`delete` 后容器、TCP、Secret、Service、Deployment、本地目录都没了；再删一次也成功 | `create-c-half-2.log`、`delete-c-2.log` |
| `verify-from-scratch.sh`（`up.sh` 已改为调用 CLI） | ✅ 删除环境、从零重建、TC-00~09 全部 PASS | `results/20261108-172558/`、`results/env/create-tenant-*.log` |

### 实现

- **14 个 Step**（简历数字 `N`）：tenants-namespace、control-plane、admin-kubeconfig、api-proxy、kubeconfig（原 02）；worker-node、join、kube-proxy、calico、images、nvml-mock、hami（原 03）；admission-policy、monitoring（原 05）。
- 每步的 `Check` 只读，进度全靠查集群和 Docker，不存状态文件。原脚本里的检查都保留了：Helm 渲染检查、swap 和 `failSwapOn`、kube-proxy 看 iptables 规则、HAMi 以节点 GPU 容量为准、拒绝上游镜像、migStrategy、准入策略用 dry-run 门禁 Pod 验证、告警规则和信号。
- K8s 操作用 client-go 加 dynamic client（server-side apply）；Docker 和 Helm 调用命令行，封装在接口后面。manifest 用 `go:embed` 编进二进制，和 bash 脚本读的是同一份文件。
- `delete` 按相反顺序执行。集群内的步骤（calico、HAMi 等）是空操作，因为删掉 TCP 时 Kamaji 会清掉租户数据。证据：tenant-c 删除后重建，只有一个新 Node（创建 1 秒），旧的 Node 对象没留下。

### 和原脚本不同的地方

- 租户 kubeconfig 改为 `$GPUTENANT_HOME/<租户>/kubeconfig`、`internal.kubeconfig`。CLI 默认 `~/.gputenant`；`scripts/lib.sh` 把 `GPUTENANT_HOME` 默认设为 `kubeconfigs/`，测试和 02 脚本跟着用新路径。
- `tenant_api_port` 改为从代理容器读端口（`docker port`）；`00-preflight.sh` 的端口检查改为只报告，因为 CLI 会跳过被占用的端口。
- HAMi 不再额外 `rollout restart`：给 device-plugin 加环境变量时改了 Pod 模板，本身就会触发滚动更新。
- Calico manifest 缓存在 `~/.cache/gputenant/`，只在内存里改 CIDR，缓存文件保持原样。
- 版本号在 Go 和 `lib.sh` 里各有一份，`TestVersionsMatchLibSh` 负责防止两边不一致。

### 过程中发现并修好的问题

1. **Ctrl-C 被报成超时。** 被中断时日志写的是 `timeout (5m0s) waiting for ...`，其实只等了 20 秒。原因是 `wait.PollUntilContextTimeout` 在外层 ctx 被取消时，返回的错误和超时一样。修复：先判断外层 `ctx.Err()`，报 `interrupted while waiting for ...`。
2. **删除后租户目录还在。** 删目录的动作放在 `kubeconfig` 步骤，但删除是逆序执行的，那时 `internal.kubeconfig` 还没删，目录非空。修复：改由最后执行的 `admin-kubeconfig` 步骤删目录，重测后目录已删除。

### 测试

- 单元测试：Step 执行框架、端口分配、kubeconfig 改写（结果确定、保留证书）、Calico CIDR（注释、已改、其他值三种状态）、kube-proxy 渲染、HAMi 渲染检查（4 种错误）、租户名校验、版本一致性，以及用假 Docker 测删除时容忍容器不存在。
- 变异测试：去掉 `pickPort` 跳过已占用端口的判断，`TestPickPort` 失败；去掉 Apply 后的复查，`TestCreateDetectsApplyWithoutEffect` 失败。

## 耗时

- 从零开通：tenant-b 3 分 24 秒（手动）、3 分 45 秒（`up.sh` 里）；tenant-a 3 分 55 秒（`up.sh` 里）。tenant-a 中断后重跑 2 分 26 秒。
- 这些不是按 `X` 的定义测的，**还不能当简历数字**：`X` 要取 3 次 `time gputenant create` 的中位数。

## 你需要亲自看的

1. `results/cli-20261108-165831/create-a-1-killed.log` 和 `create-a-2-rerun.log` 对照看：第二次哪些步骤是 skipped，从哪一步接着做。
2. `delete-c-2.log` 和终端里 delete 之后的检查输出：没有容器，管理集群里也没有 tenant-c 的对象。
3. `results/20261108-172558/summary.md`：从零重建后 TC-00~09 全部 PASS。
4. 读 `internal/tenant/steps_worker.go` 里的 `joinStep.Check`：为什么既要看 Node 对象，又要看容器里的 kubelet。

## 能讲清的问题

- 为什么 `Check` 必须只读，而且要快？（`status` 要用它，而且几秒内返回）
- 为什么不存状态文件？kill 掉以后怎么知道做到了哪一步？
- 删除为什么要逆序？为什么集群内的步骤不用单独删？
- server-side apply 和 `kubectl apply`（client-side）有什么区别？为什么重复执行结果不变？
- 中断后报错写成"超时"，会对排障造成什么误导？

## 第 2 块：`status`、`verify`、本地 e2e、PR CI

计划调整（用户已同意）：CI e2e 挪到第 3 块。它要从 GHCR 拉镜像，而且测试假设 tenant-a 是 a100（TC-07 期望单卡 40960），要先改成能在单个 t4×2 租户上跑。第 2 块的完成标准改为“PR CI 跑通、本地 e2e 通过”。

### 完成标准对照

| 项目 | 结果 | 证据（`results/cli-status-verify-20261108-204408/`，下同） |
|---|---|---|
| `status` 几秒内返回 | ✅ tenant-a 3.1 秒、tenant-b 2.7 秒，14/14；不存在的租户 0.6 秒，1/14，退出码 1 | `status-a.log`、`status-b.log`、`status-missing.log` |
| `status` 发现运行中的故障 | ✅ hami-scheduler 缩到 0，约 90 秒后 `HamiWebhookDown` firing，`status` 报 `degraded`，退出码 1 | `status-b-degraded.log` |
| `verify` 正常租户通过 | ✅ 两个租户各 14 项全过，约 57 秒 | `verify-a.log`、`verify-b-restored-2.log` |
| `verify` 发现故障 | ✅ hami-scheduler 缩到 0：4 项失败（调度器 Ready、gpumem Pod 被准入策略拒绝、信号、告警），退出码 1；恢复后全过 | `verify-b-broken-2.log` |
| GPU 参数不一致时 `create` 拒绝 | ✅ tenant-a 是 a100×1，传 t4×2 时直接报错 | `create-a-mismatch.log` |
| 本地 e2e | ✅ `scripts/e2e.sh`：两个租户 create 全部跳过（2 秒）、verify 通过、status 14/14，TC-00~09 全部 PASS | `results/e2e-20261108-210436/`、`results/20261108-210641/` |
| PR CI | 本地跑过同样的命令（gofmt、vet、`go test -race`、构建、shellcheck 0.10.0）都通过；**GitHub 上还没跑过** | `.github/workflows/ci.yml` |

### 实现

- `status`：依次执行 14 个 `Check`，每个最多 10 秒，不在第一个未完成的步骤停下。前面有步骤没完成时，后面 `Check` 的错误多半只是依赖不存在（比如还没有 kubeconfig），只记成 pending，不刷屏。步骤都完成但有状态告警 firing 时判为 degraded。
- `verify`：`04-verify-env.sh` 的全部检查，加上 gpumem Pod：必须 `schedulerName=hami-scheduler`、在本租户节点上、`hami.io/bind-phase=success`、有 `hami.io/vgpu-devices-allocated`，并且 Running。只看 Running 不够，webhook 放行时默认调度器也能把它调度起来。Pod 跑完就删，开始前先删上次中断留下的。
- 期望的 GPU 数：从租户里 nvml-mock 的 Helm release（`helm get values`）读回，`status`、`verify` 不用传参数。
- `scripts/e2e.sh`、`.github/workflows/ci.yml`、`.shellcheckrc`（关掉的 4 条都是误报，文件里写了原因）。

### 过程中发现并修好的问题

1. **超时报错里的原因被盖掉。** 第一次反向测试时，信号检查超时报 `last error: client rate limiter Wait returned an error: context deadline exceeded`，真实原因是 `up{job="hami-scheduler"}` 没有值。原因：等待到期时正在进行的请求被打断，这个错误覆盖了 `lastErr`；而“值不对”不算错误，根本没被记下。修复：等待到期后的错误不记；信号检查记下哪个信号不对。修复后报 `up{job="hami-scheduler"}="", want 1`。`TestWaitUntilKeepsRealErrorOnTimeout` 去掉修复后失败，报错和实测的一字不差。这个问题 `create` 里的等待也有，一起修了。
2. **租户坏了 `status` 还退出 0。** hami 的 `Check` 只看安装结果，调度器后来挂掉它照样为 true。改为同时看状态告警。代价：告警有 `for: 1m`，故障后约 90 秒 `status` 才能发现。

### 测试

- 新增单元测试：`Status` 不中途停下、blocked 判定；GPU values 解析；参数一致性检查（包括 helm 其他错误不能当成“没装”）；`Verify` 失败后继续；`checkHAMiScheduled`（默认调度器、绑定失败、别的节点）；`stablePods`；`StateAlertsIn`；`waitUntil` 超时保留真实原因、中断报 interrupted。
- 变异测试：去掉 schedulerName 判断，`TestCheckHAMiScheduled` 失败；去掉 blocked 判定，`TestStatusChecksEveryStep` 失败；恢复 `lastErr` 旧写法，`TestWaitUntilKeepsRealErrorOnTimeout` 失败。

### 你需要亲自看的（第 2 块）

1. `verify-b-broken-2.log`：4 项 FAIL 各自的原因，尤其是 gpumem Pod 被 `require-hami-scheduler` 拒绝，这就是默认防护在起作用。
2. `status-b-degraded.log`：步骤 14/14 done，但退出码 1。
3. 第一次的 `verify-b-broken.log` 和 `verify-b-broken-2.log` 对照看：修复前后“信号”那一行的报错。

### 能讲清的问题（第 2 块）

- `status` 为什么不在第一个未完成的步骤停下？blocked 和 error 有什么区别？
- 为什么步骤全部完成还不代表租户是好的？`status` 用告警判断有什么代价（`for: 1m`）？
- 为什么 gpumem Pod 只看 Running 不够？
- `verify` 会修改集群（重启 kube-proxy、建 Pod），为什么还叫冒烟测试，而且不能放进 `status`？
- 超时报错里只有 `context deadline exceeded`，对排障有什么误导？

## 还没做

- 推送后确认 GitHub Actions 真的跑通，然后建 PR。
- 简历数字 `X` 还没测。
