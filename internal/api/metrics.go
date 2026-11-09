package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zhexizx/gputenant/internal/tenant"
)

// Metrics 导出跨租户的指标。租户状态由后台定期刷新成快照，抓取时直接读快照：
// 一次 Inspect 要几秒，不能放在 Prometheus 的抓取请求里做。
type Metrics struct {
	backend Backend
	reg     *prometheus.Registry

	mu          sync.Mutex
	snap        map[string]tenant.Report
	lastRefresh time.Time
	refreshOK   bool

	provisionSeconds *prometheus.HistogramVec
	provisionTotal   *prometheus.CounterVec
}

var (
	labelsTenant = []string{"tenant"}

	descReady     = prometheus.NewDesc("gputenant_tenant_ready", "1 if all provisioning steps pass Check.", labelsTenant, nil)
	descDegraded  = prometheus.NewDesc("gputenant_tenant_degraded", "1 if the tenant is ready but a state alert is firing.", labelsTenant, nil)
	descStepsDone = prometheus.NewDesc("gputenant_tenant_steps_done", "Provisioning steps whose Check passes.", labelsTenant, nil)
	descSteps     = prometheus.NewDesc("gputenant_tenant_steps_total", "Provisioning steps.", labelsTenant, nil)
	descVGPUCap   = prometheus.NewDesc("gputenant_gpu_vgpu_capacity", "vGPU slots registered by HAMi on the tenant worker (nvidia.com/gpu).", labelsTenant, nil)
	descVGPUAlloc = prometheus.NewDesc("gputenant_gpu_vgpu_allocated", "vGPU slots allocated by HAMi to live Pods.", labelsTenant, nil)
	descMemCap    = prometheus.NewDesc("gputenant_gpu_memory_capacity_mib", "GPU memory registered by HAMi, MiB.", labelsTenant, nil)
	descMemAlloc  = prometheus.NewDesc("gputenant_gpu_memory_allocated_mib", "GPU memory allocated by HAMi to live Pods, MiB.", labelsTenant, nil)
	descCoresCap  = prometheus.NewDesc("gputenant_gpu_cores_capacity", "GPU compute registered by HAMi, percent (100 per card).", labelsTenant, nil)
	descCoresAll  = prometheus.NewDesc("gputenant_gpu_cores_allocated", "GPU compute allocated by HAMi to live Pods, percent.", labelsTenant, nil)
	descAlerts    = prometheus.NewDesc("gputenant_alerts_firing", "Alerts firing in the tenant's Prometheus.", labelsTenant, nil)
	descAlert     = prometheus.NewDesc("gputenant_alert_firing", "1 for each alert firing in the tenant's Prometheus.", []string{"tenant", "alertname"}, nil)
	descInspectOK = prometheus.NewDesc("gputenant_tenant_inspect_errors", "Parts of the tenant state that could not be read in the last refresh.", labelsTenant, nil)
	descRefreshTS = prometheus.NewDesc("gputenant_refresh_timestamp_seconds", "Unix time of the last completed refresh.", nil, nil)
	descRefreshOK = prometheus.NewDesc("gputenant_refresh_success", "1 if the last refresh could list tenants.", nil, nil)
)

// NewMetrics 创建指标。tasks 用来导出正在跑的任务数，可以为 nil。
func NewMetrics(b Backend) *Metrics {
	m := &Metrics{
		backend: b,
		reg:     prometheus.NewRegistry(),
		snap:    map[string]tenant.Report{},
		provisionSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gputenant_task_duration_seconds",
			Help:    "Duration of create/delete tasks run by this API instance (reset on restart).",
			Buckets: []float64{5, 15, 30, 60, 120, 180, 240, 300, 450, 600, 900},
		}, []string{"op", "result"}),
		provisionTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gputenant_tasks_total",
			Help: "create/delete tasks finished by this API instance (reset on restart).",
		}, []string{"op", "result"}),
	}
	m.reg.MustRegister(m, m.provisionSeconds, m.provisionTotal,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// RegisterTasks 导出正在跑的任务数。
func (m *Metrics) RegisterTasks(t *Tasks) {
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "gputenant_tasks_running", Help: "create/delete tasks currently running.",
	}, func() float64 { return float64(t.Running()) }))
}

// TaskFinished 记录任务结果，给 Tasks 的 onFinish 用。
func (m *Metrics) TaskFinished(t Task, d time.Duration) {
	result := "success"
	if t.State != TaskSucceeded {
		result = "failure"
	}
	m.provisionSeconds.WithLabelValues(t.Op, result).Observe(d.Seconds())
	m.provisionTotal.WithLabelValues(t.Op, result).Inc()
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Run 立即刷新一次，然后每隔 interval 刷新，直到 ctx 取消。
// 启动时立即刷新：API 重启后，指标从集群的实际状态重新算出来。
func (m *Metrics) Run(ctx context.Context, interval time.Duration) {
	for {
		m.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Refresh 重新 Inspect 所有租户，整体替换快照：已删除的租户不会留下旧指标。
func (m *Metrics) Refresh(ctx context.Context) {
	names, err := m.backend.List(ctx)
	if err != nil {
		m.mu.Lock()
		m.refreshOK = false
		m.lastRefresh = time.Now()
		m.mu.Unlock()
		return
	}
	snap := make(map[string]tenant.Report, len(names))
	for _, n := range names {
		ictx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		rep, err := m.backend.Inspect(ictx, n)
		cancel()
		if err != nil {
			rep = tenant.Report{Tenant: n, Errors: map[string]string{"inspect": err.Error()}}
		}
		snap[n] = rep
	}
	m.mu.Lock()
	m.snap, m.refreshOK, m.lastRefresh = snap, true, time.Now()
	m.mu.Unlock()
}

func (m *Metrics) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descReady, descDegraded, descStepsDone, descSteps, descVGPUCap, descVGPUAlloc,
		descMemCap, descMemAlloc, descCoresCap, descCoresAll, descAlerts, descAlert, descInspectOK, descRefreshTS, descRefreshOK} {
		ch <- d
	}
}

func (m *Metrics) Collect(ch chan<- prometheus.Metric) {
	m.mu.Lock()
	defer m.mu.Unlock()
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	if !m.lastRefresh.IsZero() {
		gauge(descRefreshTS, float64(m.lastRefresh.Unix()))
	}
	gauge(descRefreshOK, b2f(m.refreshOK))
	for name, r := range m.snap {
		gauge(descReady, b2f(r.Ready), name)
		gauge(descDegraded, b2f(r.Degraded), name)
		gauge(descStepsDone, float64(r.StepsDone), name)
		gauge(descSteps, float64(r.StepsTotal), name)
		gauge(descInspectOK, float64(len(r.Errors)), name)
		if r.Usage != nil {
			gauge(descVGPUCap, float64(r.Usage.VGPUCapacity), name)
			gauge(descVGPUAlloc, float64(r.Usage.VGPUAllocated), name)
			gauge(descMemCap, float64(r.Usage.MemCapacityMiB), name)
			gauge(descMemAlloc, float64(r.Usage.MemAllocMiB), name)
			gauge(descCoresCap, float64(r.Usage.CoresCapacity), name)
			gauge(descCoresAll, float64(r.Usage.CoresAllocated), name)
		}
		// 读不到告警时不导出 alerts_firing，免得被当成 0（一切正常）。
		if _, failed := r.Errors["alerts"]; r.Ready && !failed {
			gauge(descAlerts, float64(len(r.Alerts)), name)
			seen := map[string]bool{}
			for _, a := range r.Alerts {
				if !seen[a] {
					seen[a] = true
					gauge(descAlert, 1, name, a)
				}
			}
		}
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
