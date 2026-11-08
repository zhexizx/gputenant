# gputenant 工作总结（2026-11-08）

按 README「剩余计划」完成第 0 块，第 1 块（`gputenant create`、`delete`）代码写完并在真实环境验证。分支 `feat/cli-create-delete`，**还没提交**。

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

## 还没做

- 提交、推送、建 PR。
- `status`、`verify` 是第 2 块的内容，目前还是空壳。
