package api

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/zhexizx/gputenant/internal/tenant"
)

// collectGauge 从 Collect 的输出里找出某个指标、某个租户的值。
func collectGauge(t *testing.T, m *Metrics, name, tenantName string) prometheus.Collector {
	t.Helper()
	ch := make(chan prometheus.Metric, 100)
	m.Collect(ch)
	close(ch)
	for metric := range ch {
		if !strings.Contains(metric.Desc().String(), `"`+name+`"`) {
			continue
		}
		var d dto.Metric
		_ = metric.Write(&d)
		for _, l := range d.Label {
			if l.GetName() == "tenant" && l.GetValue() == tenantName {
				return constCollector{metric}
			}
		}
	}
	t.Fatalf("metric %s{tenant=%q} not exported", name, tenantName)
	return nil
}

type constCollector struct{ m prometheus.Metric }

func (c constCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.m.Desc() }
func (c constCollector) Collect(ch chan<- prometheus.Metric) { ch <- c.m }

func TestMetricsFromSnapshot(t *testing.T) {
	f := newFake()
	f.tenants = []string{"a", "b"}
	f.reports["a"] = tenant.Report{Tenant: "a", Ready: true, Degraded: true, StepsDone: 14, StepsTotal: 14,
		Usage:  &tenant.GPUUsage{VGPUCapacity: 10, VGPUAllocated: 3, MemCapacityMiB: 40960, MemAllocMiB: 30000, CoresCapacity: 100, CoresAllocated: 90},
		Alerts: []string{"HamiWebhookDown", "HamiWebhookFailOpen"}}
	// b 的告警读不到：不能导出 alerts_firing=0 冒充“没有告警”。
	f.reports["b"] = tenant.Report{Tenant: "b", Ready: true, StepsDone: 14, StepsTotal: 14,
		Alerts: []string{}, Errors: map[string]string{"alerts": "prometheus unreachable"}}
	m := NewMetrics(f)
	m.Refresh(context.Background())

	expect := `
# HELP gputenant_alerts_firing Alerts firing in the tenant's Prometheus.
# TYPE gputenant_alerts_firing gauge
gputenant_alerts_firing{tenant="a"} 2
# HELP gputenant_gpu_memory_allocated_mib GPU memory allocated by HAMi to live Pods, MiB.
# TYPE gputenant_gpu_memory_allocated_mib gauge
gputenant_gpu_memory_allocated_mib{tenant="a"} 30000
# HELP gputenant_tenant_degraded 1 if the tenant is ready but a state alert is firing.
# TYPE gputenant_tenant_degraded gauge
gputenant_tenant_degraded{tenant="a"} 1
gputenant_tenant_degraded{tenant="b"} 0
# HELP gputenant_tenant_inspect_errors Parts of the tenant state that could not be read in the last refresh.
# TYPE gputenant_tenant_inspect_errors gauge
gputenant_tenant_inspect_errors{tenant="a"} 0
gputenant_tenant_inspect_errors{tenant="b"} 1
`
	if err := testutil.CollectAndCompare(m, strings.NewReader(expect),
		"gputenant_alerts_firing", "gputenant_gpu_memory_allocated_mib", "gputenant_tenant_degraded", "gputenant_tenant_inspect_errors"); err != nil {
		t.Fatal(err)
	}

	// 租户删除后，下一次刷新不能留下它的旧指标。
	f.mu.Lock()
	f.tenants = []string{"b"}
	f.mu.Unlock()
	m.Refresh(context.Background())
	if n := testutil.CollectAndCount(m, "gputenant_tenant_ready"); n != 1 {
		t.Fatalf("gputenant_tenant_ready series after delete = %d, want 1", n)
	}
}
