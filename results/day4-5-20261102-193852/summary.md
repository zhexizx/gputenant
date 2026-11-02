# Day 4-5 探针结果 day4-5-20261102-193852

| 实验 | 组 | 探针 | apply rc | 耗时(s) | 已创建 | 调度器 | 节点 | 状态 | HAMi 注解数 | 最后的 Warning | apply 输出 |
|---|---|---|---|---|---|---|---|---|---|---|---|
| baseline | 对照 | over-card | 0 | 0.2 | 是 | hami-scheduler | - | Pending | 0 | FilteringFailed: no available node, 1 nodes do not meet  | pod/over-card created  |
| baseline | 对照 | gpu-only | 0 | 0.3 | 是 | hami-scheduler | tenant-b-worker | Running | 6 | -  | pod/gpu-only created  |
| baseline | 对照 | plain-probe | 0 | 0.2 | 是 | default-scheduler | tenant-b-worker | Running | 0 | -  | pod/plain-probe created  |
| baseline | 故障 | over-card | 0 | 0.2 | 是 | default-scheduler | - | Pending | 0 | FailedScheduling: 0/1 nodes are available: 1 Insufficient nvidia.com/gpucores, 1 Insufficient nvidia.com/gpumem. no new claims to deallocate, preemption: 0/1 nodes are available: 1 Preemption is not h | pod/over-card created  |
| baseline | 故障 | gpu-only | 0 | 0.2 | 是 | default-scheduler | tenant-b-worker | Failed/UnexpectedAdmissionError | 0 | UnexpectedAdmissionError: Allocate failed due to rpc error: code = Unknown desc = no node lock naming a pending pod on node tenant-b-worker, which is unexpected  | pod/gpu-only created  |
| baseline | 故障 | plain-probe | 0 | 0.2 | 是 | default-scheduler | tenant-b-worker | Running | 0 | -  | pod/plain-probe created  |
| fix-a | 对照 | over-card | 0 | 0.2 | 是 | hami-scheduler | - | Pending | 0 | FilteringFailed: no available node, 1 nodes do not meet  | pod/over-card created  |
| fix-a | 对照 | gpu-only | 0 | 0.3 | 是 | hami-scheduler | tenant-b-worker | Running | 6 | -  | pod/gpu-only created  |
| fix-a | 对照 | plain-probe | 0 | 0.2 | 是 | default-scheduler | tenant-b-worker | Running | 0 | -  | pod/plain-probe created  |
| fix-a | 故障 | over-card | 1 | 0.2 | 否 | - | - | - | - | - | Error from server (InternalError): error when creating "/home/Zecy/kamaji-hami-lab/manifests/experiments/over-card.yaml": Internal error occurred: failed calling webhook "vgpu.hami.io": failed to call |
| fix-a | 故障 | gpu-only | 1 | 0.3 | 否 | - | - | - | - | - | Error from server (InternalError): error when creating "/home/Zecy/kamaji-hami-lab/manifests/experiments/gpu-only.yaml": Internal error occurred: failed calling webhook "vgpu.hami.io": failed to call  |
| fix-a | 故障 | plain-probe | 1 | 0.3 | 否 | - | - | - | - | - | Error from server (InternalError): error when creating "/home/Zecy/kamaji-hami-lab/manifests/experiments/plain-probe.yaml": Internal error occurred: failed calling webhook "vgpu.hami.io": failed to ca |
