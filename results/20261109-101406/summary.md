# TC 结果 20261109-101406

代码版本：`05b71e6-dirty`

| TC | 结论 | 说明 |
|---|---|---|
| TC-00 | PASS | 测试框架自检（不依赖集群）; 7 checks, 0 failed |
| TC-01 | PASS | 控制面和 API 地址; 5 checks, 0 failed |
| TC-02 | PASS | 节点和 Konnectivity 隧道（tenant-a）; 4 checks, 0 failed |
| TC-03 | PASS | webhook 把 GPU Pod 交给 hami-scheduler; 3 checks, 0 failed |
| TC-04 | PASS | HAMi 完成分配和绑定，节点锁自动释放; 7 checks, 0 failed |
| TC-05 | PASS | 3 个 Pod 共享一张卡; 8 checks, 0 failed |
| TC-06 | PASS | 算力超分被拒绝（已用 90，再申请 30）; 4 checks, 0 failed |
| TC-07 | PASS | 显存超分被拒绝（申请 50000，单卡 40960）; 5 checks, 0 failed |
| TC-09 | PASS | 默认防护：准入策略和告警; 9 checks, 0 failed |
