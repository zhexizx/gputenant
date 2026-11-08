package tenant

// verify 是冒烟测试，对应 scripts/04-verify-env.sh，另加一个 gpumem Pod 的调度检查。完整的 TC 交给 e2e。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// VerifyCheck 是一项冒烟检查，失败时返回说明原因的错误。
type VerifyCheck struct {
	Name string
	Run  func(ctx context.Context) error
}

// CheckReporter 接收每项检查的结果。
type CheckReporter func(name string, err error)

// Verify 依次执行所有检查，某项失败也继续，返回失败的项数。
func Verify(ctx context.Context, checks []VerifyCheck, report CheckReporter) int {
	failed := 0
	for _, c := range checks {
		err := c.Run(ctx)
		if err != nil {
			failed++
		}
		report(c.Name, err)
		if ctx.Err() != nil {
			break
		}
	}
	return failed
}

// VerifyChecks 返回租户的全部冒烟检查。e.Cfg 的 GPU 配置要先用 LoadGPU 填好。
func VerifyChecks(e *Env) []VerifyCheck {
	v := verifier{e}
	return []VerifyCheck{
		// 先扰动：修复要扛得住 Kamaji reconcile 和 kube-proxy 重启（troubleshooting 第 3 条）。
		{"kube-proxy stable after reconcile and restart", v.reconcileAndRestart},
		{"ClusterIP rules present", v.clusterIPRules},
		{"no Kamaji-managed kube-proxy", v.noKamajiKubeProxy},
		{"API reachable", v.apiReachable},
		{"node Ready", v.nodeReady},
		{"nvidia.com/gpu allocatable", v.gpuAllocatable},
		{"tunnel (apiserver → konnectivity → kubelet)", v.tunnel},
		// 调度器崩溃时 webhook 会放行，Pod 静默退回默认调度器（troubleshooting 第 4 条）。
		{"hami-scheduler Ready", v.schedulerReady},
		{"HAMi uses local images", v.localImages},
		{"gpumem Pod scheduled by hami-scheduler and Running", v.gpumemPod},
		{"admission policy denies GPU Pods that skip the webhook", v.admission},
		{"alert rules loaded", v.rulesLoaded},
		{"alert signals healthy", v.signals},
		{"no state alert firing", v.noneFiring},
	}
}

type verifier struct{ e *Env }

func (v verifier) kube() (*Kube, error) { return v.e.TenantKube() }

const reconcileAnnotation = "lab/verified-at"

func (v verifier) reconcileAndRestart(ctx context.Context) error {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	ann := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, reconcileAnnotation, now)
	if _, err := v.e.Mgmt.Dynamic.Resource(tcpGVR).Namespace(TenantsNamespace).Patch(ctx, v.e.Cfg.Tenant,
		types.MergePatchType, []byte(ann), metav1.PatchOptions{FieldManager: fieldManager}); err != nil {
		return fmt.Errorf("trigger reconcile: %w", err)
	}
	v.e.Log("triggered Kamaji reconcile, waiting 30s")
	if err := sleep(ctx, 30*time.Second); err != nil {
		return err
	}
	k, err := v.kube()
	if err != nil {
		return err
	}
	restart := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().Format(time.RFC3339))
	if _, err := k.Clientset.AppsV1().DaemonSets("kube-system").Patch(ctx, "lab-kube-proxy",
		types.StrategicMergePatchType, []byte(restart), metav1.PatchOptions{FieldManager: fieldManager}); err != nil {
		return fmt.Errorf("restart kube-proxy: %w", err)
	}
	// 崩溃型故障启动后很快退出，等一会儿再判断。
	v.e.Log("restarted lab-kube-proxy, waiting 20s")
	if err := sleep(ctx, 20*time.Second); err != nil {
		return err
	}
	return waitUntil(ctx, 90*time.Second, "kube-proxy stable after restart", v.e.Log,
		func(ctx context.Context) (bool, error) { return kubeProxyStable(ctx, k) })
}

// kubeProxyStable：恰好一个没在删除的 kube-proxy Pod，容器 Ready 且没有重启过。
func kubeProxyStable(ctx context.Context, k *Kube) (bool, error) {
	pods, err := k.Clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "k8s-app=lab-kube-proxy"})
	if err != nil {
		return false, err
	}
	return stablePods(pods.Items) == 1, nil
}

func stablePods(pods []corev1.Pod) int {
	n := 0
	for _, p := range pods {
		if p.DeletionTimestamp != nil {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready && cs.RestartCount == 0 {
				n++
			}
		}
	}
	return n
}

func (v verifier) clusterIPRules(ctx context.Context) error {
	ok, err := kubeProxyStep{e: v.e}.Check(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no default/kubernetes rule in iptables KUBE-SERVICES")
	}
	return nil
}

func (v verifier) noKamajiKubeProxy(ctx context.Context) error {
	k, err := v.kube()
	if err != nil {
		return err
	}
	_, err = k.Clientset.AppsV1().DaemonSets("kube-system").Get(ctx, "kube-proxy", metav1.GetOptions{})
	if err == nil {
		return errors.New("Kamaji recreated the kube-proxy DaemonSet")
	}
	return ignoreNotFound(err)
}

func (v verifier) apiReachable(ctx context.Context) error {
	ok, err := apiserverReady(ctx, v.e)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("/readyz did not return ok")
	}
	return nil
}

func (v verifier) nodeReady(ctx context.Context) error {
	k, err := v.kube()
	if err != nil {
		return err
	}
	n, err := k.Clientset.CoreV1().Nodes().Get(ctx, v.e.Cfg.Worker(), metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !nodeReady(n) {
		return fmt.Errorf("node %s is not Ready", n.Name)
	}
	return nil
}

func (v verifier) gpuAllocatable(ctx context.Context) error {
	want := int64(v.e.Cfg.ExpectedGPU())
	if want == 0 {
		return errors.New("nvml-mock not installed, expected GPU count unknown")
	}
	g, err := v.e.WorkerGPU(ctx)
	if err != nil {
		return err
	}
	if g.Allocatable != want {
		return fmt.Errorf("nvidia.com/gpu=%d, want %d (%s × split %d)", g.Allocatable, want,
			GPU{v.e.Cfg.GPUProfile, v.e.Cfg.GPUCount}, gpuSplit)
	}
	v.e.Log("nvidia.com/gpu=%d (%s)", g.Allocatable, GPU{v.e.Cfg.GPUProfile, v.e.Cfg.GPUCount})
	return nil
}

// tunnel 读 konnectivity-agent 的日志：logs 要经过 apiserver → konnectivity → kubelet。
func (v verifier) tunnel(ctx context.Context) error {
	k, err := v.kube()
	if err != nil {
		return err
	}
	pods, err := k.Clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "k8s-app=konnectivity-agent"})
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning {
			continue
		}
		tail := int64(1)
		_, err := k.Clientset.CoreV1().Pods("kube-system").GetLogs(p.Name, &corev1.PodLogOptions{TailLines: &tail}).DoRaw(ctx)
		if err != nil {
			return fmt.Errorf("logs %s: %w", p.Name, err)
		}
		return nil
	}
	return errors.New("no running konnectivity-agent Pod")
}

func (v verifier) schedulerReady(ctx context.Context) error {
	k, err := v.kube()
	if err != nil {
		return err
	}
	d, err := k.Clientset.AppsV1().Deployments("kube-system").Get(ctx, "hami-scheduler", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if d.Status.ReadyReplicas != 1 {
		return fmt.Errorf("hami-scheduler readyReplicas=%d", d.Status.ReadyReplicas)
	}
	return nil
}

func (v verifier) localImages(ctx context.Context) error {
	k, err := v.kube()
	if err != nil {
		return err
	}
	return checkLocalHAMiImages(ctx, k)
}

const verifyPodName = "gputenant-verify"

// gpumemPod 建一个申请 gpumem 的 Pod：webhook 要把它交给 hami-scheduler，HAMi 要完成分配，Pod 要 Running。
// 跑完删掉，不留东西。
func (v verifier) gpumemPod(ctx context.Context) error {
	k, err := v.kube()
	if err != nil {
		return err
	}
	pods := k.Clientset.CoreV1().Pods("default")
	zero := int64(0)
	cleanup := func(ctx context.Context) error {
		err := pods.Delete(ctx, verifyPodName, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		if ignoreNotFound(err) != nil {
			return err
		}
		return waitUntil(ctx, time.Minute, "verify Pod deleted", func(string, ...any) {}, func(ctx context.Context) (bool, error) {
			_, err := pods.Get(ctx, verifyPodName, metav1.GetOptions{})
			return err != nil && ignoreNotFound(err) == nil, nil
		})
	}
	// 上次被中断留下的 Pod 先删掉，避免拿旧结果当这次的结果。
	if err := cleanup(ctx); err != nil {
		return err
	}
	defer cleanup(context.WithoutCancel(ctx))

	objs, err := DecodeYAML(verifyPod)
	if err != nil {
		return err
	}
	if err := k.ApplyObject(ctx, objs[0]); err != nil {
		return err
	}
	var last *corev1.Pod
	err = waitUntil(ctx, 2*time.Minute, "verify Pod Running", v.e.Log, func(ctx context.Context) (bool, error) {
		p, err := pods.Get(ctx, verifyPodName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		last = p
		return p.Status.Phase == corev1.PodRunning, nil
	})
	if err != nil {
		if last != nil {
			return fmt.Errorf("%w (phase=%s, schedulerName=%s)", err, last.Status.Phase, last.Spec.SchedulerName)
		}
		return err
	}
	return checkHAMiScheduled(last, v.e.Cfg.Worker())
}

// checkHAMiScheduled 确认 Pod 是 hami-scheduler 调度的，而且 HAMi 完成了 GPU 分配。
// 只看 Running 不够：webhook 放行时 Pod 会被默认调度器调度，也能 Running。
func checkHAMiScheduled(p *corev1.Pod, node string) error {
	var problems []string
	if p.Spec.SchedulerName != "hami-scheduler" {
		problems = append(problems, "schedulerName="+p.Spec.SchedulerName)
	}
	if p.Spec.NodeName != node {
		problems = append(problems, "nodeName="+p.Spec.NodeName)
	}
	if got := p.Annotations["hami.io/bind-phase"]; got != "success" {
		problems = append(problems, "hami.io/bind-phase="+got)
	}
	if p.Annotations["hami.io/vgpu-devices-allocated"] == "" {
		problems = append(problems, "no hami.io/vgpu-devices-allocated")
	}
	if len(problems) > 0 {
		return fmt.Errorf("not scheduled by HAMi: %s", strings.Join(problems, ", "))
	}
	return nil
}

func (v verifier) admission(ctx context.Context) error {
	k, err := v.kube()
	if err != nil {
		return err
	}
	ok, err := vapDenies(ctx, k)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("GPU Pod with hami.io/webhook=ignore was not denied")
	}
	return nil
}

func (v verifier) rulesLoaded(ctx context.Context) error {
	ok, err := promRulesLoaded(ctx, v.e)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("not all of %s loaded", strings.Join(GuardrailAlerts, " "))
	}
	return nil
}

// 刚重启过 kube-proxy，信号可能要等一次抓取才更新，所以下面两项轮询。
func (v verifier) signals(ctx context.Context) error {
	var bad string
	err := waitUntil(ctx, 2*time.Minute, "tunnel probe, hami-scheduler and agent signals healthy", v.e.Log,
		func(ctx context.Context) (bool, error) {
			b, err := promBadSignal(ctx, v.e)
			if err != nil {
				return false, err
			}
			bad = b
			return b == "", nil
		})
	if err != nil && bad != "" {
		return fmt.Errorf("%w: %s", err, bad)
	}
	return err
}

func (v verifier) noneFiring(ctx context.Context) error {
	var firing []string
	err := waitUntil(ctx, 2*time.Minute, "no state alert firing", v.e.Log, func(ctx context.Context) (bool, error) {
		all, err := FiringAlerts(ctx, v.e)
		if err != nil {
			return false, err
		}
		firing = StateAlertsIn(all)
		return len(firing) == 0, nil
	})
	if err != nil && len(firing) > 0 {
		return fmt.Errorf("%w: firing %s", err, strings.Join(firing, " "))
	}
	return err
}

// StateAlertsIn 只保留故障状态告警。HamiWebhookFailOpen 是事件型，故障恢复后还会持续约 5 分钟，不算。
func StateAlertsIn(alerts []string) []string {
	var out []string
	for _, a := range alerts {
		if slices.Contains(StateAlerts, a) && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("interrupted: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}
