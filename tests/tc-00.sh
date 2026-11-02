#!/usr/bin/env bash
# 测试框架自检，不需要集群：用假的 tk 模拟 kubectl 的各种情况，确认 fetch_json 和
# check_pending_on_hami 在出错时判为 FAIL，不会因为输出为空而误判通过。
source "$(dirname "$0")/lib.sh"
tc_begin TC-00 "测试框架自检（不依赖集群）"

POD_PENDING='{"spec":{"schedulerName":"hami-scheduler"},"status":{"phase":"Pending"}}'
POD_BOUND='{"spec":{"schedulerName":"hami-scheduler","nodeName":"tenant-a-worker"},"status":{"phase":"Running"}}'

# 每个场景在子 shell 里运行，有自己的计数器，不影响本 TC 的结论。
# run_case <描述> <期望 "rc checks fails"> <函数>
run_case() {
  local desc="$1" want="$2" out; shift 2
  out=$(CHECKS=0; FAILS=0; "$@" 2>&1; rc=$?; echo "COUNTS $rc $CHECKS $FAILS")
  printf '\n## %s\n' "$desc"
  sed '$d; s/^/    | /' <<<"$out"
  check_eq "$desc" "$(tail -1 <<<"$out")" "COUNTS $want"
}

case_not_found() {
  tk() { echo 'Error from server (NotFound): pods "x" not found' >&2; return 1; }
  fetch_json v "$A" get pod x
}
case_api_down() {
  tk() { echo 'dial tcp 127.0.0.1:16443: connect: connection refused' >&2; return 1; }
  fetch_json v "$A" get node "$NODE_A"
}
case_not_json() {
  tk() { echo 'not json'; }
  fetch_json v "$A" get node "$NODE_A"
}
case_warning_on_stderr() {
  tk() { echo 'Warning: something is deprecated' >&2; echo '{"metadata":{"annotations":{}}}'; }
  fetch_json v "$A" get node "$NODE_A" && jq -e '.metadata.annotations == {}' <<<"$v" >/dev/null
}
case_pending_api_down() {
  tk() { echo 'connection refused' >&2; return 1; }
  check_pending_on_hami gpu-share-4
}
case_pending_but_bound() {
  tk() { echo "$POD_BOUND"; }
  check_pending_on_hami gpu-share-4
}
case_pending_ok() {
  tk() { echo "$POD_PENDING"; }
  check_pending_on_hami gpu-share-4
}

run_case "fetch_json：资源不存在时 FAIL" "1 1 1" case_not_found
run_case "fetch_json：API 不通时 FAIL" "1 1 1" case_api_down
run_case "fetch_json：输出不是 JSON 时 FAIL" "1 1 1" case_not_json
run_case "fetch_json：stderr 有警告时 JSON 不被污染" "0 0 0" case_warning_on_stderr
run_case "check_pending_on_hami：API 不通时 FAIL，不判通过" "1 1 1" case_pending_api_down
run_case "check_pending_on_hami：Pod 已被调度时 FAIL" "0 3 2" case_pending_but_bound
run_case "check_pending_on_hami：正常 Pending 时 3 项通过" "0 3 0" case_pending_ok
tc_end
