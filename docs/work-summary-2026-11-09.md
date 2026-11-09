# gputenant 工作总结（2026-11-09）

第 3 块（Gin API、GHCR 发布、CI e2e、Grafana 看板）完成：PR #9 合并，tag `v0.1.0` 触发的 release 和之后的 CI e2e 在 GitHub 上都通过。

## 完成标准对照

| 完成标准 | 结果 | 证据 |
|---|---|---|
| API 重启后不会误报成功 | ✅ 用 API 开通 tenant-c，join 做完后 `kill -9`。重启后：旧任务 `GET /tasks/<id>` 返回 404；`GET /tenants/tenant-c` 返回 `ready:false`、8/14；指标 `gputenant_tenant_ready{tenant="tenant-c"} 0`，任务计数为空。再次 POST，前 8 步 skipped，从 calico 续做，3 分 22 秒成功 | `results/api-20261109-095837/02~04-*.log` |
| 同一租户只能有一个任务 | ✅ 开通中再 POST、DELETE 都返回 409，并指向正在跑的任务 | `02-create-and-kill.log` |
| 删除后不留旧指标 | ✅ DELETE 后 `GET` 返回 404，容器已删除，`/metrics` 里没有 tenant-c 了 | `05-delete.log` |
| `gputenant` 镜像能用 | ✅ 本地构建的镜像（235MB，docker CLI 28.5.1、helm v3.19.0、HAMi chart）以当前用户运行 `serve`，从零开通 tenant-d 3 分 35 秒，宿主机 CLI 能读同一个租户，删除干净；写出的文件属主是 1000:1000 | `results/image-20261109-100723/` |
| GHCR 发布 | ✅ tag `v0.1.0` 触发 release：hami 4 分 21 秒、nvml-mock 3 分 28 秒、gputenant 1 分 42 秒，3 个 job 都成功，没有跳过构建步骤 | release run 42546815733 |
| CI e2e 跑通 | ✅ GitHub Actions 上通过，01:48–01:57 UTC，约 9 分钟，所有步骤 success。合并时 push 触发的第 1 次运行在镜像发布前，拉 hami 镜像报 `manifest unknown`，属预期；重新运行后通过。本地模拟结果见右侧 | e2e run 42545273247；`results/e2e-20261109-101301/`、`results/20261109-101406/` |
| 看板可以演示 | ✅ 15 个面板的查询逐个在 Prometheus 上验证有数据，分配率和手算一致（tenant-b：显存 8000/32768=0.244）；hami-scheduler 缩到 0 后约 100 秒，降级租户、firing 告警表都显示 tenant-b / HamiWebhookDown。**面板在浏览器里的样子还没人看过** | `results/dashboard-20261109-101717/` |

## 实现

- **`tenant.Inspect`**：把 `status` 的组装逻辑挪进 `internal/tenant`，CLI 和 API 共用。新增 GPU 分配量：容量来自节点注解 `hami.io/node-nvidia-register`，已分配量来自 Pod 注解 `hami.io/vgpu-devices-allocated`（容器之间用 `;`、同一容器的多张卡用 `:` 分隔，每张卡 `uuid,类型,显存,算力`），只算没结束、没在删除的 Pod。
- **`internal/api`**：
  - Gin 路由：`POST /tenants`、`DELETE /tenants/:name` 返回 202 和任务（带 `Location`）；`GET /tenants/:name` 每次重新跑 Check；`GET /tasks/:id`；`/metrics`、`/healthz`。
  - 任务只在内存里：同一租户同时只能有一个，返回给 HTTP 的是副本，任务里的 panic 记成失败、不会永远占着租户；日志最多保留 200 行。
  - 指标：后台每 30 秒对所有租户跑一次 `Inspect`，整体替换快照（删掉的租户不会留下旧指标）。读不到告警时不导出告警数，免得被当成 0。启动时立即刷新一次，重启后的指标从集群实际状态算出来。
  - `serve` 默认只监听 `127.0.0.1`：API 能开通、删除租户，镜像里还挂着 `docker.sock`。
- **镜像和发布**：多阶段 `Dockerfile`；`release.yml` 打 `v*` tag 时发布 `gputenant`，hami、nvml-mock 按固定 commit 从源码构建，tag 是 commit 前 12 位，已存在就跳过。`TestDockerfileMatchesLibSh` 检查 Dockerfile 和两个 workflow 里的 commit、helm 版本与 `lib.sh` 一致。
- **CI e2e**（`e2e.yml`）：从 GHCR 拉镜像并改名为 `hami:local`、`nvml-mock:local`，按固定 commit 取 HAMi chart，`01-management.sh`，然后 `E2E_TENANTS=tenant-a scripts/e2e.sh 00 01 02 03 04 05 06 07 09`，结果作为 artifact 上传。TC-09 和 TC-01 一样改为按 `TC_TENANTS` 循环。
- **看板**（`deploy/`）：docker compose 起 API（宿主机网络）、socat 代理、Prometheus、Grafana（匿名只读，看板自动加载）；`scripts/dashboard.sh up|down`。

## 和 README 原设计不同的地方（已写进 README）

1. CI e2e 建 tenant-a（a100×1），不是 t4×2：TC-04~06 要 3 个 10000 MiB 的 Pod 共享一张卡，t4 每张卡只有 16384 MiB。
2. 告警不经 apiserver 代理查询：代理要经过 Konnectivity 隧道，隧道断开时正好查不到。继续用节点内 curl ClusterIP。
3. API 在 WSL 上要再经一层 socat 代理（troubleshooting 第 13 条）。

## 过程中发现并修好的问题

1. **`gpu` 字段序列化成 `Profile`、`Count`**：`GPU` 结构体没有 JSON tag。另外没有告警时 `firingAlerts` 是 `null` 而不是 `[]`。第一次实测时发现，已修。
2. **测试脚本被自己中止**：`curl | head -c 0` 让 curl 写失败（rc=23），lib.sh 开着 `errexit`，整个脚本退出。测试脚本改为 `set +e`，自己判断结果。
3. **容器里的 API 在 WSL 访问不到**：见 troubleshooting 第 13 条。第一次用 8081 端口时 curl 拿到的 404 其实来自 ModelServe 的进程。

## 测试

- 单元测试（新增）：API 的 202/409/400/404、任务失败和 panic、**重启后旧任务 404 且租户按 Inspect 报 not ready**、开通中的租户能查状态；指标快照（告警读不到时不导出、删除后不留旧序列）；分配注解和节点注解解析；Dockerfile 和 workflow 版本一致性。`go test -race` 通过。
- 变异测试：快照改成合并而不是替换 → `TestMetricsFromSnapshot` 失败；告警读不到时也导出 0 → 同一个测试失败；去掉“租户已有任务”的判断 → `TestCreateReturns202ThenConflict` 失败；把 e2e.yml 的 HAMi commit 改掉 → `TestDockerfileMatchesLibSh` 失败。
- 静态检查：gofmt、vet、shellcheck 0.10.0、actionlint 1.7.7、`docker compose config` 都通过。

## 你需要亲自看的

1. `results/api-20261109-095837/03-after-restart.log`：`kill -9` 之后，任务 404、租户 8/14、`tenant_ready` 为 0。这是第 3 块的核心完成标准。
2. 打开 http://127.0.0.1:3000 看看板（`scripts/dashboard.sh up` 后，环境还在时）：面板布局、单位、颜色是否合理。我只验证了查询有数据，没看过画面。
3. `internal/api/metrics.go` 的 `Collect`：为什么读不到告警时干脆不导出，而不是导出 0。

## 能讲清的问题

- 为什么任务放在内存里也不会“重启后误报成功”？状态的唯一来源是什么？
- 为什么 `/metrics` 不在抓取请求里现算，而是读后台快照？快照为什么要整体替换？
- 分配率和利用率有什么区别？为什么这个项目只能做分配率？（nvml-mock 没有真实负载）
- 为什么 API 默认只监听 127.0.0.1？挂载 `docker.sock` 意味着什么？
- 为什么 CI 不从源码构建 HAMi，而是拉 GHCR 上按 commit 打 tag 的镜像？
- Docker Desktop 的“宿主机网络”到底是谁的网络？

## 合并后

1. ✅ 打 tag `v0.1.0`，release 发布了三个镜像。
2. ✅ CI e2e 重新运行后通过。CI 的 TC 明细在那次运行的 `e2e-results` artifact 里（需要登录 GitHub 下载），没有存进仓库。
3. 还没做：GHCR 的包默认私有，想让别人能 `docker pull`，要在包设置里改成 Public。
4. 已知问题：合并到 `main` 时 e2e 会和 release 同时或更早触发；只要 hami、nvml-mock 的固定 commit 不变，镜像已经在 GHCR 上，以后不会再出现。改 commit 时要先跑 release 再跑 e2e。
