#!/usr/bin/env bash
# 检查依赖；helm、jq 校验后装到项目内的 .tools/bin（不需要 sudo）。
source "$(dirname "$0")/lib.sh"
mkdir -p "$TOOLS_BIN" "$CACHE_DIR" "$KUBECONFIG_DIR"

for c in docker kind kubectl curl sha256sum tar; do
  command -v "$c" >/dev/null || die "缺少 $c"
done

if [[ ! -x "$TOOLS_BIN/helm" ]]; then
  log "安装 helm $HELM_VERSION"
  f="helm-$HELM_VERSION-linux-amd64.tar.gz"
  curl -fsSL "https://get.helm.sh/$f" -o "$CACHE_DIR/$f"
  want=$(curl -fsSL "https://get.helm.sh/$f.sha256sum" | cut -d' ' -f1)
  echo "$want  $CACHE_DIR/$f" | sha256sum -c -
  tar -xzf "$CACHE_DIR/$f" -C "$CACHE_DIR" linux-amd64/helm
  install -m 0755 "$CACHE_DIR/linux-amd64/helm" "$TOOLS_BIN/helm"
fi

if [[ ! -x "$TOOLS_BIN/jq" ]]; then
  log "安装 jq $JQ_VERSION"
  base="https://github.com/jqlang/jq/releases/download/jq-$JQ_VERSION"
  curl -fsSL "$base/jq-linux-amd64" -o "$CACHE_DIR/jq-linux-amd64"
  want=$(curl -fsSL "$base/sha256sum.txt" | awk '$2 == "jq-linux-amd64" {print $1}')
  [[ -n "$want" ]] || die "sha256sum.txt 里没有 jq-linux-amd64"
  echo "$want  $CACHE_DIR/jq-linux-amd64" | sha256sum -c -
  install -m 0755 "$CACHE_DIR/jq-linux-amd64" "$TOOLS_BIN/jq"
fi

log "环境检查"
docker info 2>/dev/null | grep -E 'Server Version|Cgroup Driver'
free -h
df -h "$HOME" | tail -1
watches=$(sysctl -n fs.inotify.max_user_watches)
instances=$(sysctl -n fs.inotify.max_user_instances)
echo "inotify: watches=$watches instances=$instances"
if ((watches < 524288 || instances < 512)); then
  die "inotify 限制过低，请运行: sudo sysctl -w fs.inotify.max_user_watches=524288 fs.inotify.max_user_instances=512"
fi
helm version --short
jq --version
