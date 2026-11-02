#!/usr/bin/env bash
# 测试框架：每个 TC 把命令和真实输出写到 $RUN_DIR/TC-xx.log。
# 结论只由 check 决定：任何一项失败或没有执行到 tc_end，都不算通过。
source "$(dirname "${BASH_SOURCE[0]}")/../scripts/lib.sh"
set +e

RUN_DIR="${RUN_DIR:-$ROOT/results/manual-$(date +%Y%m%d-%H%M%S)}"
BUSYBOX=busybox:1.37
A=tenant-a
B=tenant-b
NODE_A=tenant-a-worker
FAILS=0
CHECKS=0

tc_begin() {
  TC="$1"; mkdir -p "$RUN_DIR"
  exec > >(tee "$RUN_DIR/$TC.log") 2>&1
  echo "# $TC: $2"
  echo "# 开始: $(date -Is)"
}
# run <命令...>：显示命令和输出，不影响结论。
run() { printf '\n$ %s\n' "$*"; "$@"; }
pass() { CHECKS=$((CHECKS + 1)); echo "CHECK PASS: $1"; }
fail() { CHECKS=$((CHECKS + 1)); FAILS=$((FAILS + 1)); echo "CHECK FAIL: $1"; }
# check <描述> <命令...>：命令成功才算通过。
check() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }
# check_eq <描述> <实际> <期望>
check_eq() {
  if [[ "$2" == "$3" ]]; then pass "$1 (= $3)"; else fail "$1: 实际 '$2'，期望 '$3'"; fi
}
# fetch <变量名> <租户> <kubectl 参数...>：把 -o json 的结果存进变量。kubectl 失败就记一次 FAIL 并返回 1。
# 期望值为空的检查必须先用它取数据，否则 API 不通时空输出也会被判为通过。
fetch() {
  local __var="$1" __t="$2" __out; shift 2
  if __out=$(tk "$__t" "$@" -o json 2>&1); then
    printf -v "$__var" '%s' "$__out"
  else
    fail "kubectl $*: $(head -c 200 <<<"$__out")"
    return 1
  fi
}
tc_end() {
  local verdict=PASS
  ((CHECKS > 0 && FAILS == 0)) || verdict=FAIL
  printf '\n# 结束: %s\nRESULT %s %s (%d checks, %d failed)\n' "$(date -Is)" "$TC" "$verdict" "$CHECKS" "$FAILS"
  [[ "$verdict" == PASS ]]; exit $?
}

gpu_pod() { # gpu_pod <名称> <gpumem> <gpucores>
  cat <<EOF | tk "$A" apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: $1
spec:
  containers:
    - name: app
      image: $BUSYBOX
      command: ["sleep", "infinity"]
      env:
        - name: CUDA_DISABLE_CONTROL
          value: "true"
      resources:
        limits:
          nvidia.com/gpu: 1
          nvidia.com/gpumem: $2
          nvidia.com/gpucores: $3
EOF
}
# 3 个共享 Pod，TC-03~06 共用。已经存在的不会重建。
ensure_share_pods() {
  local i
  for i in 1 2 3; do
    tk "$A" get pod "gpu-share-$i" >/dev/null 2>&1 || gpu_pod "gpu-share-$i" 10000 30
    run tk "$A" wait "pod/gpu-share-$i" --for=condition=Ready --timeout=120s
  done
}
# 轮询 Pod 事件，直到出现某个 reason 模式。
wait_event() { # wait_event <pod> <正则> <秒>
  local deadline=$((SECONDS + $3))
  while ((SECONDS < deadline)); do
    tk "$A" describe pod "$1" | sed -n '/Events/,$p' | grep -qE "$2" && return 0
    sleep 3
  done
  return 1
}
