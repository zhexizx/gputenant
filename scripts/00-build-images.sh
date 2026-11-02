#!/usr/bin/env bash
# 按固定 commit 构建 nvml-mock:local 和 hami:local。可重复运行。
source "$(dirname "$0")/lib.sh"
mkdir -p "$SRC_DIR"

checkout() {
  local url="$1" dir="$2" commit="$3"
  [[ -d "$dir/.git" ]] || git clone "$url" "$dir"
  git -C "$dir" fetch --quiet origin "$commit"
  git -C "$dir" checkout --quiet --detach "$commit"
  git -C "$dir" submodule update --init --recursive
  echo "$dir @ $(git -C "$dir" rev-parse HEAD)"
}
checkout https://github.com/NVIDIA/k8s-test-infra.git "$SRC_DIR/k8s-test-infra" "$K8S_TEST_INFRA_COMMIT"
checkout https://github.com/Project-HAMi/HAMi.git "$SRC_DIR/HAMi" "$HAMI_COMMIT"

log "构建 nvml-mock:local"
docker build -q -t nvml-mock:local -f "$SRC_DIR/k8s-test-infra/deployments/nvml-mock/Dockerfile" "$SRC_DIR/k8s-test-infra"
log "构建 hami:local"
docker build -q -t hami:local -f "$SRC_DIR/HAMi/docker/Dockerfile" "$SRC_DIR/HAMi"
docker images --format '{{.Repository}}:{{.Tag}} {{.ID}} {{.Size}}' | grep -E '^(nvml-mock|hami):local'
