package tenant

// 默认防护：准入策略和 Prometheus 告警，对应 scripts/05-tenant-guardrails.sh。
// 依据 docs/failure-report.md：隧道或 HAMi webhook 故障时 GPU Pod 会被静默放行，所以总是部署，没有关闭开关。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---- 13. 准入策略 require-hami-scheduler ----

type admissionStep struct {
	e *Env
	noDelete
}

func (s admissionStep) Name() string { return "admission-policy" }

const vapMessage = "GPU Pod must use hami-scheduler"

// Check 用 dry-run 创建门禁 Pod：它带 hami.io/webhook=ignore 会跳过 webhook，必须被策略拒绝。
// 只看策略对象存不存在不够：创建后 apiserver 要过一会儿才开始执行它。
func (s admissionStep) Check(ctx context.Context) (bool, error) {
	k, err := s.e.TenantKube()
	if err != nil {
		return false, err
	}
	return vapDenies(ctx, k)
}

func vapDenies(ctx context.Context, k *Kube) (bool, error) {
	objs, err := DecodeYAML(vapGatePod)
	if err != nil {
		return false, err
	}
	err = k.DryRunCreate(ctx, objs[0])
	return err != nil && strings.Contains(err.Error(), vapMessage), nil
}

func (s admissionStep) Apply(ctx context.Context) error {
	k, err := s.e.TenantKube()
	if err != nil {
		return err
	}
	if err := k.ApplyYAML(ctx, requireHamiScheduler); err != nil {
		return err
	}
	return waitUntil(ctx, 90*time.Second, "admission policy denies GPU Pods that skip the webhook", s.e.Log, s.Check)
}

// ---- 14. Prometheus 和告警 ----

type monitoringStep struct {
	e *Env
	noDelete
}

func (s monitoringStep) Name() string { return "monitoring" }

func (s monitoringStep) Check(ctx context.Context) (bool, error) {
	ok, err := helmDeployed(ctx, s.e.Helm, s.e.Cfg.Kubeconfig(), "monitoring", "prometheus")
	if err != nil || !ok {
		return false, err
	}
	k, err := s.e.TenantKube()
	if err != nil {
		return false, err
	}
	if _, err := k.Clientset.RbacV1().ClusterRoleBindings().Get(ctx, "prometheus-tunnel-probe", metav1.GetOptions{}); err != nil {
		return false, ignoreNotFound(err)
	}
	return promRulesLoaded(ctx, s.e)
}

func (s monitoringStep) Apply(ctx context.Context) error {
	values, err := os.CreateTemp("", "gputenant-prometheus-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(values.Name())
	if _, err := values.Write(prometheusValues); err != nil {
		return err
	}
	values.Close()
	if _, err := s.e.Helm.Run(ctx, "upgrade", "--install", "prometheus", "prometheus",
		"--repo", PrometheusChartRepo, "--version", PrometheusChartVer,
		"-n", "monitoring", "--create-namespace", "--kubeconfig", s.e.Cfg.Kubeconfig(),
		"-f", values.Name(), "--wait", "--timeout", "8m"); err != nil {
		return err
	}
	k, err := s.e.TenantKube()
	if err != nil {
		return err
	}
	// tunnel-probe 经 apiserver 的节点代理抓取，需要 nodes/proxy 权限。
	if err := k.ApplyYAML(ctx, prometheusRBAC); err != nil {
		return err
	}
	if err := waitUntil(ctx, 2*time.Minute, "alert rules loaded: "+strings.Join(GuardrailAlerts, " "), s.e.Log,
		func(ctx context.Context) (bool, error) { return promRulesLoaded(ctx, s.e) }); err != nil {
		return err
	}
	if err := waitUntil(ctx, 4*time.Minute, "tunnel probe, hami-scheduler and agent signals healthy", s.e.Log,
		func(ctx context.Context) (bool, error) { return promSignalsOK(ctx, s.e) }); err != nil {
		return err
	}
	return waitUntil(ctx, 2*time.Minute, "no state alert firing", s.e.Log,
		func(ctx context.Context) (bool, error) {
			firing, err := FiringAlerts(ctx, s.e)
			if err != nil {
				return false, err
			}
			for _, a := range firing {
				if slices.Contains(StateAlerts, a) {
					return false, nil
				}
			}
			return true, nil
		})
}

// ---- Prometheus 查询：在节点内经 ClusterIP 访问。port-forward 也经过隧道，故障时用不了。----

func promAPI(ctx context.Context, e *Env, path string, curlArgs ...string) ([]byte, error) {
	k, err := e.TenantKube()
	if err != nil {
		return nil, err
	}
	svc, err := k.Clientset.CoreV1().Services("monitoring").Get(ctx, "prometheus-server", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if svc.Spec.ClusterIP == "" {
		return nil, fmt.Errorf("prometheus-server has no ClusterIP")
	}
	args := append([]string{"exec", e.Cfg.Worker(), "curl", "-sS", "-m", "10", "-G",
		"http://" + svc.Spec.ClusterIP + "/api/v1/" + path}, curlArgs...)
	out, err := e.Docker.Run(ctx, nil, args...)
	return []byte(out), err
}

func promRulesLoaded(ctx context.Context, e *Env) (bool, error) {
	out, err := promAPI(ctx, e, "rules")
	if err != nil {
		return false, err
	}
	var resp struct {
		Data struct {
			Groups []struct {
				Rules []struct {
					Name string `json:"name"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return false, err
	}
	loaded := map[string]bool{}
	for _, g := range resp.Data.Groups {
		for _, r := range g.Rules {
			loaded[r.Name] = true
		}
	}
	for _, a := range GuardrailAlerts {
		if !loaded[a] {
			return false, nil
		}
	}
	return true, nil
}

// promQuery 返回所有结果值，用逗号分隔。
func promQuery(ctx context.Context, e *Env, q string) (string, error) {
	out, err := promAPI(ctx, e, "query", "--data-urlencode", "query="+q)
	if err != nil {
		return "", err
	}
	var resp struct {
		Data struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", err
	}
	var vals []string
	for _, r := range resp.Data.Result {
		if len(r.Value) == 2 {
			vals = append(vals, fmt.Sprint(r.Value[1]))
		}
	}
	return strings.Join(vals, ","), nil
}

// promSignalsOK：告警依赖的三个信号都是正常值。
func promSignalsOK(ctx context.Context, e *Env) (bool, error) {
	for _, q := range []string{
		`up{job="tunnel-probe"}`,
		`up{job="hami-scheduler"}`,
		`konnectivity_network_proxy_agent_open_server_connections`,
	} {
		v, err := promQuery(ctx, e, q)
		if err != nil {
			return false, err
		}
		if v != "1" {
			return false, nil
		}
	}
	return true, nil
}

// FiringAlerts 返回租户 Prometheus 里正在 firing 的告警名。
func FiringAlerts(ctx context.Context, e *Env) ([]string, error) {
	out, err := promAPI(ctx, e, "alerts")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Alerts []struct {
				State  string            `json:"state"`
				Labels map[string]string `json:"labels"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, err
	}
	var firing []string
	for _, a := range resp.Data.Alerts {
		if a.State == "firing" {
			firing = append(firing, a.Labels["alertname"])
		}
	}
	return firing, nil
}
