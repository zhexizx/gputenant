#!/usr/bin/env bash
source "$(dirname "$0")/lib.sh"
tc_begin TC-03 "webhook 把 GPU Pod 交给 hami-scheduler"
ensure_share_pods
for i in 1 2 3; do
  check_eq "gpu-share-$i schedulerName" "$(tk "$A" get pod "gpu-share-$i" -o jsonpath='{.spec.schedulerName}')" hami-scheduler
done
tc_end
