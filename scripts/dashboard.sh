#!/usr/bin/env bash
# 启动或停止 gputenant API + Prometheus + Grafana（deploy/docker-compose.yml）。
# 用法: dashboard.sh up [镜像]|down。镜像默认 gputenant:dev（本地 docker build 出来的），可传 ghcr.io/zhexizx/gputenant:<tag>。
source "$(dirname "$0")/lib.sh"
compose() { docker compose -f "$ROOT/deploy/docker-compose.yml" "$@"; }
case "${1:-}" in
  up)
    # Prometheus 抓取地址写死了 kind 网关，不一致时直接报错，免得看板一片空白。
    gw=$(docker network inspect kind | jq -r '.[0].IPAM.Config[] | select(.Subnet | test("^[0-9.]+/")) | .Gateway')
    [[ "$gw" == 172.18.0.1 ]] || die "kind 网关是 $gw，不是 172.18.0.1：改 deploy/prometheus.yml 和 docker-compose.yml 里的地址"
    export GPUTENANT_IMAGE="${2:-gputenant:dev}" UID GID DOCKER_GID
    GID=$(id -g); DOCKER_GID=$(stat -c %g /var/run/docker.sock)
    compose up -d
    # 配置和看板是 bind mount：切换 git 分支时目录可能被删掉再重建，旧容器还挂着已删除的目录，
    # Grafana 会报 Failed to load home dashboard。这两个容器没有要保留的状态，每次都重建。
    compose up -d --force-recreate prometheus grafana
    for _ in $(seq 60); do curl -sf http://127.0.0.1:18081/healthz >/dev/null && break; sleep 1; done
    curl -sf http://127.0.0.1:18081/healthz >/dev/null || die "API 没有起来：docker logs gputenant-api-1"
    echo "API        http://127.0.0.1:18081"
    echo "Prometheus http://127.0.0.1:9090"
    echo "Grafana    http://127.0.0.1:3000（匿名只读）"
    ;;
  down) compose down ;;
  *) die "用法: dashboard.sh up [镜像]|down" ;;
esac
