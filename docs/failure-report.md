# 降级实验报告：Konnectivity 隧道中断时的 GPU 调度

实验设计来自项目计划（仓库外）的 Day 4–5。实验在 2026-11-02 进行，环境是 `tenant-b`：2 张模拟 T4，每张 16384 MiB，`nvidia.com/gpu=20`。

以下第 1~4 条结论来自 `results/day4-5-20261102-194838/`，这一轮四个步骤都返回 0；2026-11-03 加入默认防护后重跑了一轮（`results/day4-5-20261103-113034/`，五个步骤都返回 0），结论一致。webhook 故障的结论见文末“2026-11-03 补充”。原始输出在各步骤的子目录里，探针汇总在 `summary.md`。

## 结论

1. **隧道断开时，默认配置（`failurePolicy=Ignore`）会静默放行 GPU Pod，交给 `default-scheduler`，HAMi 的核算被绕过。** 这次没有观察到超分，Pod 被下游拦了下来：
   - `over-card` 申请了 `gpumem`/`gpucores`，节点不上报这两种资源，默认调度器让它一直 Pending。
   - `gpu-only` 只申请 `nvidia.com/gpu`，被默认调度器调度到节点，随后 device-plugin 的 Allocate 失败，Pod 进入终态 `Failed/UnexpectedAdmissionError`，原因是 `no node lock naming a pending pod`。
   - `kubectl apply` 返回成功，耗时约 0.2 秒，用户看不到任何报错。
2. **方案 A（`failurePolicy=Fail`）在隧道断开时会拒绝所有新建的 Pod，包括不申请 GPU 的。** 报错是 `failed calling webhook "vgpu.hami.io" ... No agent available`。
3. **方案 B（ValidatingAdmissionPolicy）只拒绝 GPU Pod，并且给出明确原因。** 报错是 `GPU Pod must use hami-scheduler`。普通 Pod 照常创建；隧道正常时 GPU Pod 也照常创建，由 HAMi 调度。计划的 3 项检查全部通过。
4. **计划里的告警 `KonnectivityAgentUnavailable` 在隧道断开 351 秒里一直没有触发**：agent Pod 一直可用，`kube_daemonset_status_number_unavailable` 始终是 0。新增的两条规则都真实触发过，恢复后也都解除了：
   - `ApiserverToNodePathDown`：经过隧道的端到端探测，阻断后 86 秒 firing。
   - `KonnectivityAgentDisconnected`：agent 到 server 的连接数，阻断后 136 秒 firing。

**建议：** 采用方案 B，配合 `ApiserverToNodePathDown` 和 `KonnectivityAgentDisconnected` 两条告警，不采用方案 A。2026-11-03 起，方案 B 和告警已经是默认部署（`scripts/05-tenant-guardrails.sh`），并补上了 `initContainers` 检查和两条 webhook 告警，见文末。

## 方法

| 项 | 做法 |
|---|---|
| 隧道阻断 | 用 `nsenter` 在 konnectivity-agent Pod 的网络命名空间里，对 OUTPUT 链加 `-d 172.18.255.201 --dport 8132 -j DROP`。在节点 FORWARD 链上阻断不可靠，见 troubleshooting 第 7 条 |
| 断开门禁 | API `/readyz` 正常，并且 `kubectl logs ds/konnectivity-agent` 连续 3 次失败，才创建测试 Pod。观察结束时再确认一次隧道仍然断开、agent PID 没变（三轮都是 2114） |
| 对照组 | 隧道正常时先跑同样的探针。`over-card` 必须被 HAMi 拒绝，`gpu-only` 必须由 HAMi 调度并运行，否则不做故障组。三轮的对照组都有效 |
| 探针 | `over-card`（`gpumem: 20000`，超过单卡、不超过两张卡）、`gpu-only`（只申请 `nvidia.com/gpu: 1`）、`plain-probe`（不申请 GPU） |
| 观察 | 创建后观察 60 秒，记录 apply 结果和耗时、`schedulerName`、节点、状态、HAMi 注解、按 UID 过滤的 Warning 事件 |
| 恢复 | 每轮结束后删除探针、删除阻断规则、把策略改回 `Ignore`、删除准入策略，再检查隧道、规则、策略和 GPU 容量 |
| 前置 | `scripts/04-verify-env.sh tenant-b` 通过，确认 hami-scheduler Ready，排除调度器崩溃造成的放行（troubleshooting 第 4 条） |

## 探针结果

| 实验 | 隧道 | over-card | gpu-only | plain-probe |
|---|---|---|---|---|
| 基线 | 正常 | 已创建；`hami-scheduler`；Pending，`FilteringFailed` | 已创建；`hami-scheduler`；Running，6 个 HAMi 注解 | Running |
| 基线 | 断开 | 已创建（0.2s）；`default-scheduler`；Pending，`Insufficient nvidia.com/gpucores, nvidia.com/gpumem` | 已创建（0.2s）；`default-scheduler`；调度到节点后 `Failed/UnexpectedAdmissionError`，0 个 HAMi 注解 | Running |
| 方案 A | 正常 | 同基线 | 同基线 | Running |
| 方案 A | 断开 | **被拒绝**（0.3s）：`failed calling webhook ... No agent available` | **被拒绝**（0.3s），原因相同 | **被拒绝**（0.3s），原因相同 |
| 方案 B | 正常 | 同基线 | 同基线 | Running |
| 方案 B | 断开 | **被拒绝**（0.3s）：`GPU Pod must use hami-scheduler` | **被拒绝**（0.3s），原因相同 | Running |

## 分析

**调度路径。** 正常时 apiserver 经隧道调用 HAMi webhook，把 `schedulerName` 改成 `hami-scheduler`，再由 HAMi 按单卡核算。隧道断开后，konnectivity-server 上没有可用的 agent，webhook 调用立即失败，没有等满 `timeoutSeconds: 10`。`Ignore` 模式下 Pod 原样放行，`schedulerName` 保持默认值。

**基线没有超分的原因。** 这和 HAMi 的实现以及节点上报的资源有关，不是故障处理设计出来的：
- 节点只上报 `nvidia.com/gpu`，不上报 `nvidia.com/gpumem` 和 `gpucores`，所以申请了这两种资源的 Pod 过不了默认调度器。
- 只申请 `nvidia.com/gpu` 的 Pod 能被调度，但 HAMi device-plugin 在 Allocate 时要找 HAMi 调度器留下的节点锁，找不到就报错，kubelet 把 Pod 置为 Failed。
- 这个 Pod 是终态，不会重试。用户要到 Pod 失败后才会发现，而不是在创建时。

**方案 A 的代价。** webhook 的规则匹配所有 Pod 的 CREATE 和 UPDATE，namespaceSelector 也只排除带 `hami.io/webhook=ignore` 标签的命名空间，所以隧道断开期间租户里任何新 Pod 都创建不了，包括系统组件和控制器重建的 Pod。这次只测了 CREATE，没有测 UPDATE。

**方案 B 为什么有效。** ValidatingAdmissionPolicy 在 apiserver 进程内执行，不经过隧道。webhook 正常时会先把 `schedulerName` 改掉，所以策略检查能通过；webhook 被跳过时 `schedulerName` 还是 `default-scheduler`，策略就会拒绝。隧道正常时它不影响任何 Pod。

## 告警

| 规则 | 表达式 | 结果 |
|---|---|---|
| `KonnectivityAgentUnavailable`（计划） | `kube_daemonset_status_number_unavailable{daemonset="konnectivity-agent"} > 0` | 351 秒内没有触发 |
| `KonnectivityAgentDisconnected` | `konnectivity_network_proxy_agent_open_server_connections < 1 or absent(...)` | 09:01:12Z pending，09:02:12Z firing（阻断后 136 秒） |
| `ApiserverToNodePathDown` | `up{job="tunnel-probe"} == 0` | 09:00:27Z pending，09:01:27Z firing（阻断后 86 秒） |

- 阻断时间是 09:00:12Z（本地 20:00:12），时间来自 `alert/alerts-history.json`。
- 三条规则都是 `for: 1m`，抓取和评估间隔都是 15 秒。
- 隧道正常时，三个信号都是正常值：连接数 1、探测 `up` 1、不可用数 0，也没有告警。恢复后三个信号都回到正常值，告警全部解除。
- `tunnel-probe` 经 apiserver 的节点代理（`/api/v1/nodes/<node>/proxy/metrics`）抓取 kubelet，和 webhook 调用走同一条路：apiserver → konnectivity → 节点。chart 自带的 `kubernetes-nodes` 直连 kubelet，不经过隧道，不能用来探测。
- 计划的规则没有触发，原因是阻断网络不会让 agent Pod 变成不可用：agent 没有 readiness 探针，liveness 探针看的是本地 `/healthz`。所以不能说“Pod 可用性告警能发现隧道故障”。
- Prometheus 查询是在节点内通过 ClusterIP 发出的，没有用 `port-forward`，因为它也依赖隧道。

## 局限

- GPU 是 nvml-mock 模拟的。`UnexpectedAdmissionError` 来自真实运行的 HAMi device-plugin（commit `2b8f4e29`），但其他版本或真实驱动下的行为没有验证。
- 每种组合只跑了一轮有效实验，每轮观察 60 秒。第二轮（`results/day4-5-20261102-193852/`）的基线和方案 A 结果和这一轮一致。
- 只模拟了“agent 到 server 的连接被丢弃”这一种故障。server 仍以为 agent 在线但数据不通（黑洞）时，webhook 调用可能会等满 10 秒超时，这种情况没有测。
- ~~方案 B 的 `matchConditions` 只检查 `containers`，不检查 `initContainers`。~~ 2026-11-03 已修复并测试，见文末。带 `hami.io/webhook=ignore` 标签的 GPU Pod 会跳过 webhook，也会被方案 B 拒绝，TC-09 用这一点做 dry-run 测试。
- 方案 A 在 UPDATE 操作上的影响没有测。
- 告警的触发时间受抓取间隔、评估间隔和 `for: 1m` 影响，换参数后会不同。

## 实验记录

| 运行 | 结果 |
|---|---|
| `results/day4-5-20261102-173829/` | 在节点 FORWARD 链上阻断，4 分钟内隧道一直没断，门禁拦住了，没有创建故障组 Pod（troubleshooting 第 7 条） |
| `results/day4-5-20261102-193852/` | 改为在 agent 命名空间阻断。基线和方案 A 有效；方案 B 被门禁脚本的 bug 中止（troubleshooting 第 8 条），告警没有运行 |
| `results/day4-5-20261102-194838/` | 四个步骤全部完成，`overall=0`，本报告第 1~4 条结论依据这一轮 |
| `results/day4-5-20261103-111733/` | 和环境重建写在同一行命令里，前置检查的 `pgrep` 匹配到这行命令，判定有冲突，没有执行（troubleshooting 第 9 条） |
| `results/day4-5-20261103-111859/` | 前三步通过；webhook 故障一开始就中止：上一步的隧道告警还没解除（troubleshooting 第 10 条） |
| `results/day4-5-20261103-113034/` | 加入默认防护和 `gpu-init` 探针后，五个步骤全部完成，`overall=0`。其中 webhook 故障这一步的 `HamiWebhookFailOpen` 触发不能归因，见下文 |
| `results/webhook-down-20261103-115651/` | 单独重跑 webhook 故障，开始前等所有告警解除，`rc=0`，webhook 故障的结论依据这一轮 |
| `results/day4-5-20261109-162805/` | 复查 `guard`：和 `04-verify-env.sh` 写在同一行启动，前置检查没有误报（troubleshooting 第 9 条）。前四步都返回 0，探针结果和前两轮一致；告警这一步下载 Prometheus chart 时 GitHub 返回 504，没有执行 |
| `results/day4-5-20261109-164827/` | 单独重跑告警，同样因为 chart 下载 504 中止，环境已自动恢复 |
| `results/day4-5-20261109-165724/` | 单独重跑告警，两条检查都通过（见下文“告警耗时的三轮对比”）；结束时恢复 Prometheus 默认配置又遇到 504，之后用 `experiments/restore.sh` 恢复 |

## 复现

```bash
./experiments/run.sh          # 环境检查 → 基线 → 方案 A → 方案 B → webhook 故障 → 告警
./experiments/webhook-down.sh # 只跑 webhook 故障
./experiments/restore.sh      # 被强行中断后恢复
```

2026-11-03 起，Prometheus 和准入策略是默认部署。实验会临时去掉准入策略、叠加实验规则，结束时恢复为默认配置并检查。

## 2026-11-03 补充：默认防护、initContainers 和 webhook 故障

### 默认部署

`scripts/up.sh` 在每个租户的节点接入之后运行 `scripts/05-tenant-guardrails.sh`，部署：

- 准入策略 `require-hami-scheduler`（`manifests/guardrails/require-hami-scheduler.yaml`）。
- Prometheus（chart 29.40.3）和 4 条告警（`manifests/guardrails/prometheus-values.yaml`）：

| 规则 | 类型 | 发现什么 |
|---|---|---|
| `ApiserverToNodePathDown` | 状态 | apiserver 经隧道到节点的路径不通 |
| `KonnectivityAgentDisconnected` | 状态 | agent 到 konnectivity-server 没有连接 |
| `HamiWebhookDown` | 状态 | `hami-scheduler`（webhook 所在进程）的指标端口抓不到，或者没有抓取目标 |
| `HamiWebhookFailOpen` | 事件 | apiserver 记录到 HAMi webhook 调用失败后按 `Ignore` 放行，隧道和 webhook 两种故障都覆盖；最后一次放行后约 5 分钟解除 |

不再部署 `KonnectivityAgentUnavailable`，因为隧道断开时它不会触发；`alert.sh` 用 `manifests/experiments/prom-experiment-rules.yaml` 临时叠加它来复现这一点。

从零重建后，TC-09 在两个租户上检查准入策略生效、4 条规则已加载、信号正常、没有 firing 的故障状态告警（`results/20261103-111609/`，TC-00~09 全部通过）。

### initContainers

- 修复前先确认：HAMi webhook 对只在 init 容器里申请 GPU 的 Pod 也会把 `schedulerName` 改成 `hami-scheduler`，所以把 init 容器加进检查，不会误拒正常的 Pod。
- `matchConditions` 改为同时检查 `containers` 和 `initContainers`，`limits` 和 `requests` 都看。
- TC-09 用 dry-run 验证：跳过 webhook、只在 init 容器申请 GPU 的 Pod 被拒绝；正常的 init 容器 GPU Pod 照常允许。
- 变异测试（`results/guardrails-mutation-20261103-104921/`）：在 tenant-a 换回旧表达式后，TC-09 只有这一项失败；换回新表达式后全部通过。
- 隧道断开时的实测：新增的 `gpu-init` 探针在方案 B 下被明确拒绝；没有准入策略时，它被默认调度器调度后以 `UnexpectedAdmissionError` 失败，和 `gpu-only` 一样（`results/day4-5-20261103-113034/`）。
- 附带观察：隧道正常时，`gpu-init` 和 `gpu-only` 同时创建，`gpu-init` 先收到一次 `BindingFailed: node lock contention`，随后调度成功并运行。这是 HAMi 对同一节点的绑定加锁，和 mock 无关。

### HAMi webhook 本身挂掉，隧道正常

方法：webhook 由 `hami-scheduler` 提供，把它缩到 0，确认隧道正常，分别在去掉、装回准入策略时跑同样的 4 个探针，再观察告警，最后恢复。开始前等所有告警解除，告警只有 `activeAt` 晚于缩容才算这次触发的。结果来自 `results/webhook-down-20261103-115651/`，前一轮（`results/day4-5-20261103-113034/webhook-down/`）的探针结果一致。

| 准入策略 | over-card | gpu-only | gpu-init | plain-probe |
|---|---|---|---|---|
| 去掉 | 已创建；`default-scheduler`；Pending | 已创建；`default-scheduler`；`Failed/UnexpectedAdmissionError` | 同 gpu-only | Running |
| 装回 | **被拒绝**：`GPU Pod must use hami-scheduler` | **被拒绝**，原因相同 | **被拒绝**，原因相同 | Running |

结论：

1. **webhook 挂掉和隧道断开的结果一样**：`failurePolicy=Ignore` 时 GPU Pod 被静默放行，`kubectl apply` 返回成功，Pod 随后 Pending 或启动失败。
2. **准入策略在两种故障下都有效**，只拒绝 GPU Pod，普通 Pod 不受影响。
3. **两条隧道告警不会触发**（缩容后观察 196 秒都是 inactive），只靠它们发现不了 webhook 故障，所以新增了两条：
   - `HamiWebhookDown`：`activeAt` 在缩容后 26 秒，`for: 1m`，约 86 秒后 firing。
   - `HamiWebhookFailOpen`：`activeAt` 在缩容后 26 秒，随第一批探针的放行触发。apiserver 的 `apiserver_admission_webhook_fail_open_count` 从 24 增加到 32，对应这一轮的 8 次 Pod 创建，包括后来被准入策略拒绝的 4 次：放行发生在 webhook 阶段，校验在它之后。
4. 前一轮里 `HamiWebhookFailOpen` 的 `activeAt` 比缩容早 149 秒，是上一步隧道实验留下的，这一轮之前的检查把它误判为通过。之后改为开始前等它解除，并要求 `activeAt` 晚于缩容（troubleshooting 第 11 条）。

### 告警耗时的三轮对比

| 告警 | 2026-11-02 | 2026-11-03 | 2026-11-09 |
|---|---|---|---|
| `ApiserverToNodePathDown`（阻断后 firing） | 86 秒 | 116 秒 | 105 秒 |
| `KonnectivityAgentDisconnected`（阻断后 firing） | 136 秒 | 134 秒 | 138 秒 |
| `KonnectivityAgentUnavailable` | 351 秒内未触发 | 349 秒内未触发 | 339 秒内未触发 |

三轮都是 `for: 1m`、抓取和评估间隔 15 秒，脚本每 15 秒左右查一次，所以 firing 时间有十几秒的误差。

### 局限

- `HamiWebhookDown` 看的是 `hami-scheduler` 进程的指标端口，发现不了“进程在、webhook 处理逻辑坏了”；`HamiWebhookFailOpen` 能覆盖这种情况，但只在有 Pod 创建时才触发。
- `HamiWebhookFailOpen` 为了不漏掉计数器第一次出现，在 Prometheus 启动后的前 5 分钟内不判断“新出现的计数器”，这段时间里第一次放行会漏报。
- webhook 故障只模拟了“没有可用的 webhook 后端”（缩到 0），没有模拟 webhook 超时或返回错误。
