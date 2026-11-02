#!/usr/bin/env bash
source "$(dirname "$0")/lib.sh"
tc_begin TC-02 "节点和 Konnectivity 隧道（$A）"
run tk "$A" get nodes -o wide
run tk "$A" -n kube-system get pods
run tk "$A" get mutatingwebhookconfigurations
tk "$A" get mutatingwebhookconfigurations -o yaml | grep -E ' name:|failurePolicy|timeoutSeconds'
check_eq "节点 Ready" "$(tk "$A" get node "$NODE_A" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" True
check_eq "konnectivity-agent Ready" "$(tk "$A" -n kube-system get ds konnectivity-agent -o jsonpath='{.status.numberReady}')" 1
# kubectl logs 要走 apiserver → konnectivity → kubelet，能取到日志才说明隧道真的通。
printf '\n$ kubectl -n kube-system logs ds/konnectivity-agent --tail=5\n'
if logs=$(tk "$A" -n kube-system logs ds/konnectivity-agent --tail=5 2>&1); then
  echo "$logs"; pass "经隧道读取节点日志"
else
  echo "$logs"; fail "经隧道读取节点日志"
fi
check "hami-webhook 已注册" tk "$A" get mutatingwebhookconfiguration hami-webhook
echo "failurePolicy=$(tk "$A" get mutatingwebhookconfiguration hami-webhook -o jsonpath='{.webhooks[0].failurePolicy}')"
tc_end
