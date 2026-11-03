# kamaji-hami-lab 工作总结（2026-11-03）

依据 `多租户GPU平台-总计划.md` 的“11/12 上午：第二天收尾”，完成 3 项任务。分支 `feat/default-guardrails`。

## 结果

| 任务 | 完成标准 | 证据 |
|---|---|---|
| 1. 方案 B 补 `initContainers` | init 容器申请 GPU、webhook 被跳过时被拒绝；改回旧表达式时测试失败 | TC-09；变异测试 `results/guardrails-mutation-20261103-104921/`；隧道断开时 `gpu-init` 被拒绝（`results/day4-5-20261103-113034/fix-b/`） |
| 2. 方案 B 和告警加进默认部署 | 从零重建后，两个租户都有准入策略和告警规则，测试确认生效 | `results/20261103-111609/`：从零重建后 TC-00~09 全部通过；`results/env/05-tenant-*-guardrails.log` |
| 3. HAMi webhook 本身挂掉、隧道正常 | 结论写进报告；告警真实触发一次 | `results/webhook-down-20261103-115651/`，`rc=0`；结论见 `docs/failure-report.md` 文末 |

完整实验（基线、方案 A、方案 B、webhook 故障、告警）在加入默认防护后重跑了一轮，五步都返回 0（`results/day4-5-20261103-113034/`）。

## 主要结论

- **webhook 挂掉和隧道断开的结果一样：** 默认的 `failurePolicy=Ignore` 下，GPU Pod 被静默放行，随后 Pending 或以 `UnexpectedAdmissionError` 失败。
- **准入策略在两种故障下都只拒绝 GPU Pod**，包括只在 init 容器里申请 GPU 的 Pod；普通 Pod 不受影响。
- **隧道告警发现不了 webhook 故障**，所以新增了 `HamiWebhookDown` 和 `HamiWebhookFailOpen`。后者基于 apiserver 的 `apiserver_admission_webhook_fail_open_count`，隧道和 webhook 两种故障都能覆盖。

## 改动

| 路径 | 内容 |
|---|---|
| `scripts/05-tenant-guardrails.sh` | 新增：部署准入策略、Prometheus 和告警，并等到生效 |
| `scripts/up.sh` | 节点接入后运行 05 |
| `scripts/04-verify-env.sh` | 增加默认防护的检查 |
| `scripts/lib.sh` | 默认防护的检查函数：准入策略、Prometheus 查询、规则、信号、firing |
| `manifests/guardrails/` | 从 `manifests/experiments/` 移来；准入策略补 `initContainers`；Prometheus 去掉不会触发的规则，增加 webhook 的抓取和两条告警 |
| `manifests/experiments/` | 新增 `gpu-init` 探针，和只给实验用的 `prom-experiment-rules.yaml` |
| `experiments/` | 默认带防护后：基线和方案 A 临时去掉准入策略，恢复时装回；`alert.sh` 叠加实验规则，恢复时装回默认配置；新增 `webhook-down.sh` |
| `tests/tc-09.sh`、`tests/run-all.sh` | 新增 TC-09：默认防护 |
| `docs/failure-report.md`、`docs/troubleshooting.md` | 补充结论和第 9~12 条排障记录 |

## 过程中的问题

详情见 `docs/troubleshooting.md` 第 9~12 条：

1. 实验和环境重建写在同一行命令里，前置检查匹配到了自己的命令行。
2. 上一步的隧道告警还没解除，webhook 实验就中止了。
3. 事件型告警是上一步留下的，却被判为这次触发的。改为按 `activeAt` 判断后重跑。
4. WSL 里拉取 ghcr.io 的 chart 偶发失败，重跑通过。

## 注意

- 测试结果的代码版本是 `5f356ab-dirty`，这些改动还没有提交。
- 每轮实验都会临时修改 tenant-b 的配置；结束时都已恢复为默认配置，并检查过。
