#!/usr/bin/env bash
# 演示租户内多用户按配额共享 GPU，给看板的“每张卡的显存/算力”提供数据。
# tenant-a（1×A100）：demo-team-a、demo-team-b 两个用户共享同一张卡；
# tenant-b（2×T4）：demo-team-c 一个用户用一张卡，另一张卡空着。
# 每个用户一个命名空间，配额 20000 MiB 显存、50% 算力。TC-10 跑完会删掉自己的命名空间，所以演示用这个脚本。
# 用法: demo-teams.sh up|down
source "$(dirname "$0")/lib.sh"
IMAGE=busybox:1.37

# 租户 命名空间（用户）
TEAMS=(
  "tenant-a demo-team-a"
  "tenant-a demo-team-b"
  "tenant-b demo-team-c"
)
# 租户 命名空间 Pod 显存(MiB) 算力(%)
PODS=(
  "tenant-a demo-team-a a1 12000 20"
  "tenant-a demo-team-a a2 6000 15"
  "tenant-a demo-team-b b1 10000 25"
  "tenant-a demo-team-b b2 4000 10"
  "tenant-b demo-team-c c1 8000 30"
)

team() {
  cat <<EOF
---
apiVersion: v1
kind: Namespace
metadata: {name: $1, labels: {app.kubernetes.io/part-of: gputenant-demo}}
---
apiVersion: v1
kind: ResourceQuota
metadata: {name: gpu, namespace: $1}
spec:
  hard:
    limits.nvidia.com/gpumem: "20000"
    limits.nvidia.com/gpucores: "50"
EOF
}

pod() {
  cat <<EOF
apiVersion: v1
kind: Pod
metadata: {name: $2, namespace: $1}
spec:
  containers:
    - name: app
      image: $IMAGE
      command: [sleep, infinity]
      env: [{name: CUDA_DISABLE_CONTROL, value: "true"}]
      resources:
        limits: {nvidia.com/gpu: 1, nvidia.com/gpumem: $3, nvidia.com/gpucores: $4}
EOF
}

case "${1:-}" in
  up)
    for spec in "${TEAMS[@]}"; do
      read -r t ns <<<"$spec"
      team "$ns" | tk "$t" apply -f -
    done
    # HAMi 通过 informer 读到新配额要一点时间，太早提交会按“没有配额”处理。
    sleep 5
    for spec in "${PODS[@]}"; do
      read -r t ns name mem cores <<<"$spec"
      pod "$ns" "$name" "$mem" "$cores" | tk "$t" apply -f -
    done
    for spec in "${PODS[@]}"; do
      read -r t ns name _ <<<"$spec"
      tk "$t" -n "$ns" wait "pod/$name" --for=condition=Ready --timeout=120s || die "$t $ns/$name 没有 Running"
    done
    echo "看板约 30 秒后更新（API 每 30 秒刷新一次）。用完运行: $0 down"
    ;;
  down)
    for spec in "${TEAMS[@]}"; do
      read -r t ns <<<"$spec"
      tk "$t" delete namespace "$ns" --ignore-not-found --wait=true
    done
    ;;
  *) die "用法: demo-teams.sh up|down" ;;
esac
