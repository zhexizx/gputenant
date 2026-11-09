# gputenant：CLI 和 API 在同一个镜像里。运行时要挂载 docker.sock，并用宿主机网络
# （管理集群的 kubeconfig 指向 127.0.0.1，租户 API 经宿主机上的 socat 代理端口访问）。
# 版本和 scripts/lib.sh 保持一致：HAMI_COMMIT、HELM_VERSION。

FROM golang:1.26.2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY manifests ./manifests
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gputenant ./cmd/gputenant

# HAMi chart 按固定 commit 取出，create 时用它安装 HAMi。
FROM alpine/git:v2.49.1 AS chart
ARG HAMI_COMMIT=2b8f4e29c48bca376d4f6123d7c6ae7627aef579
RUN git init /hami && cd /hami \
 && git remote add origin https://github.com/Project-HAMi/HAMi.git \
 && git fetch --depth 1 origin "${HAMI_COMMIT}" \
 && git checkout --quiet FETCH_HEAD \
 && test -f charts/hami/Chart.yaml

FROM docker:28.5.1-cli AS dockercli
FROM alpine/helm:3.19.0 AS helm

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=chart /hami/charts/hami /opt/gputenant/hami-chart
COPY --from=build /out/gputenant /usr/local/bin/gputenant
ENV GPUTENANT_HAMI_CHART=/opt/gputenant/hami-chart \
    GPUTENANT_HOME=/data \
    XDG_CACHE_HOME=/data/.cache \
    HELM_CACHE_HOME=/data/.cache/helm \
    HELM_CONFIG_HOME=/data/.config/helm
EXPOSE 8080
ENTRYPOINT ["gputenant"]
CMD ["serve", "--addr", "0.0.0.0:8080"]
