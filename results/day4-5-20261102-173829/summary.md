# Day 4-5 探针结果 day4-5-20261102-173829

| 实验 | 组 | 探针 | apply rc | 耗时(s) | 已创建 | 调度器 | 节点 | 状态 | HAMi 注解数 | 最后的 Warning | apply 输出 |
|---|---|---|---|---|---|---|---|---|---|---|---|
| baseline | 对照 | over-card | 0 | 0.3 | 是 | hami-scheduler | - | Pending | 0 | FilteringFailed: no available node, 1 nodes do not meet  | pod/over-card created  |
| baseline | 对照 | gpu-only | 0 | 0.3 | 是 | hami-scheduler | tenant-b-worker | Running | 6 | -  | pod/gpu-only created  |
| baseline | 对照 | plain-probe | 0 | 0.3 | 是 | default-scheduler | tenant-b-worker | Running | 0 | -  | pod/plain-probe created  |
