#!/usr/bin/env bash
# 默认防护：准入策略 require-hami-scheduler，以及 Prometheus 和告警。可重复运行。
# 依据 docs/failure-report.md：隧道或 HAMi webhook 故障时，GPU Pod 会被静默放行，所以每个租户默认都要有。
# 用法: 05-tenant-guardrails.sh tenant-a|tenant-b
source "$(dirname "$0")/lib.sh"
t="${1:-}"; check_tenant "$t"

log "准入策略 require-hami-scheduler"
tk "$t" apply -f "$GUARDRAILS_DIR/require-hami-scheduler.yaml"
wait_until 90 "准入策略已生效（跳过 webhook 的 GPU Pod 被拒绝）" vap_denies "$t" || die "准入策略没有生效"

log "Prometheus chart $PROMETHEUS_CHART_VERSION"
helm upgrade --install prometheus prometheus --repo "$PROMETHEUS_CHART_REPO" --version "$PROMETHEUS_CHART_VERSION" \
  -n monitoring --create-namespace --kubeconfig "$(tenant_kubeconfig "$t")" \
  -f "$GUARDRAILS_DIR/prometheus-values.yaml" --wait --timeout 8m
tk "$t" apply -f "$GUARDRAILS_DIR/prometheus-rbac.yaml"
tk "$t" -n monitoring get pods -o wide

wait_until 120 "告警规则已加载：${GUARDRAIL_ALERTS[*]}" prom_rules_loaded "$t" || die "告警规则没有加载"
wait_until 240 "告警依赖的信号都是正常值" prom_signals_ok "$t" || die "隧道探测、hami-scheduler 或 agent 指标不正常"
wait_until 120 "没有 firing 的告警" prom_none_firing "$t" || die "环境正常时就有告警在 firing"
echo "$t 默认防护已部署"
