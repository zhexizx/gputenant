package tenant

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// checkStep 的 Check 返回固定结果。
type checkStep struct {
	name string
	done bool
	err  error
}

func (f checkStep) Name() string                        { return f.name }
func (f checkStep) Check(context.Context) (bool, error) { return f.done, f.err }
func (f checkStep) Apply(context.Context) error         { return nil }
func (f checkStep) Delete(context.Context) error        { return nil }

func TestStatusChecksEveryStep(t *testing.T) {
	boom := errors.New("boom")
	got := Status(context.Background(), []Step{
		checkStep{name: "a", done: true},
		checkStep{name: "b", err: boom}, // 前面都完成了：错误要报出来
		checkStep{name: "c", done: true},
		checkStep{name: "d", err: errors.New("no kubeconfig")}, // 前面有没完成的：记为 blocked
	})
	if len(got) != 4 {
		t.Fatalf("got %d states, want 4 (must not stop at first unfinished step)", len(got))
	}
	if !got[0].Done || got[0].Err != nil {
		t.Errorf("a: %+v", got[0])
	}
	if got[1].Done || !errors.Is(got[1].Err, boom) || got[1].Blocked {
		t.Errorf("b: %+v, want error reported", got[1])
	}
	if !got[2].Done {
		t.Errorf("c: %+v, want done even after an earlier failure", got[2])
	}
	if got[3].Done || got[3].Err != nil || !got[3].Blocked {
		t.Errorf("d: %+v, want blocked without error", got[3])
	}
}

func TestParseGPUValues(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want GPU
		ok   bool
	}{
		{`{"gpu":{"count":2,"profile":"t4"},"image":{"tag":"local"}}`, GPU{"t4", 2}, true},
		{`{"gpu":{"count":"1","profile":"a100"}}`, GPU{"a100", 1}, true},
		{`{"image":{"tag":"local"}}`, GPU{}, false},
		{`{"gpu":{"profile":"t4"}}`, GPU{}, false},
		{`not json`, GPU{}, false},
	} {
		got, err := parseGPUValues(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseGPUValues(%s) = %v, %v; want %v ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

// fakeHelm 按子命令返回固定输出。
type fakeHelm struct {
	out string
	err error
}

func (f fakeHelm) Run(context.Context, ...string) (string, error) { return f.out, f.err }

func TestCheckGPUMatches(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Tenant: "t1", Home: dir, GPUProfile: "t4", GPUCount: 2}
	if err := writePrivate(cfg.Kubeconfig(), []byte("x")); err != nil {
		t.Fatal(err)
	}
	installed := `{"gpu":{"count":1,"profile":"a100"}}`
	e := &Env{Cfg: cfg, Helm: fakeHelm{out: installed}}
	if err := e.CheckGPUMatches(context.Background()); err == nil || !strings.Contains(err.Error(), "a100×1") {
		t.Fatalf("mismatch not reported: %v", err)
	}
	e.Cfg.GPUProfile, e.Cfg.GPUCount = "a100", 1
	if err := e.CheckGPUMatches(context.Background()); err != nil {
		t.Fatalf("matching config rejected: %v", err)
	}
	// 新租户还没装 nvml-mock：不拦。
	e.Helm = fakeHelm{err: errors.New("helm get: exit status 1: Error: release: not found")}
	if err := e.CheckGPUMatches(context.Background()); err != nil {
		t.Fatalf("missing release rejected: %v", err)
	}
	// 其他 helm 错误要报出来，不能当成“没装”。
	e.Helm = fakeHelm{err: errors.New("cluster unreachable")}
	if err := e.CheckGPUMatches(context.Background()); err == nil {
		t.Fatal("helm error swallowed")
	}
}

func TestLoadGPUWithoutKubeconfig(t *testing.T) {
	e := &Env{Cfg: Config{Tenant: "t1", Home: t.TempDir()}, Helm: fakeHelm{err: errors.New("must not be called")}}
	if err := e.LoadGPU(context.Background()); err != nil || e.Cfg.GPUCount != 0 {
		t.Fatalf("LoadGPU = %v, count %d; want nil, 0", err, e.Cfg.GPUCount)
	}
}

func TestVerifyContinuesAfterFailure(t *testing.T) {
	var ran []string
	check := func(name string, err error) VerifyCheck {
		return VerifyCheck{name, func(context.Context) error { ran = append(ran, name); return err }}
	}
	var reported []string
	failed := Verify(context.Background(), []VerifyCheck{
		check("a", nil), check("b", errors.New("x")), check("c", nil), check("d", errors.New("y")),
	}, func(name string, err error) { reported = append(reported, name) })
	if failed != 2 || len(ran) != 4 || len(reported) != 4 {
		t.Fatalf("failed=%d ran=%v reported=%v", failed, ran, reported)
	}
}

func TestCheckHAMiScheduled(t *testing.T) {
	good := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				"hami.io/bind-phase":             "success",
				"hami.io/vgpu-devices-allocated": "GPU-0,NVIDIA,1000,10:;",
			}},
			Spec: corev1.PodSpec{SchedulerName: "hami-scheduler", NodeName: "t1-worker"},
		}
	}
	if err := checkHAMiScheduled(good(), "t1-worker"); err != nil {
		t.Fatalf("good pod rejected: %v", err)
	}
	// webhook 放行时 Pod 由默认调度器调度，也能 Running，但没有 HAMi 的分配。
	bypassed := good()
	bypassed.Spec.SchedulerName = "default-scheduler"
	bypassed.Annotations = nil
	err := checkHAMiScheduled(bypassed, "t1-worker")
	if err == nil || !strings.Contains(err.Error(), "schedulerName=default-scheduler") {
		t.Fatalf("bypassed pod accepted: %v", err)
	}
	failedBind := good()
	failedBind.Annotations["hami.io/bind-phase"] = "failed"
	if err := checkHAMiScheduled(failedBind, "t1-worker"); err == nil {
		t.Fatal("failed bind accepted")
	}
	otherNode := good()
	otherNode.Spec.NodeName = "t2-worker"
	if err := checkHAMiScheduled(otherNode, "t1-worker"); err == nil {
		t.Fatal("pod on another node accepted")
	}
}

func TestStablePods(t *testing.T) {
	now := metav1.Now()
	pod := func(ready bool, restarts int32, deleting bool) corev1.Pod {
		p := corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Ready: ready, RestartCount: restarts}}}}
		if deleting {
			p.DeletionTimestamp = &now
		}
		return p
	}
	if n := stablePods([]corev1.Pod{pod(true, 0, false), pod(true, 0, true)}); n != 1 {
		t.Errorf("terminating pod counted: %d", n)
	}
	if n := stablePods([]corev1.Pod{pod(true, 1, false)}); n != 0 {
		t.Errorf("restarted pod counted as stable: %d", n)
	}
	if n := stablePods([]corev1.Pod{pod(false, 0, false)}); n != 0 {
		t.Errorf("unready pod counted: %d", n)
	}
}

func TestStateAlertsIn(t *testing.T) {
	got := StateAlertsIn([]string{"HamiWebhookFailOpen", "HamiWebhookDown", "Watchdog", "HamiWebhookDown"})
	if len(got) != 1 || got[0] != "HamiWebhookDown" {
		t.Fatalf("got %v, want only HamiWebhookDown once", got)
	}
}

// 等待到期时正在进行的请求会被打断，打断产生的错误不能盖掉之前的真实原因。
func TestWaitUntilKeepsRealErrorOnTimeout(t *testing.T) {
	old := pollInterval
	pollInterval = 10 * time.Millisecond
	defer func() { pollInterval = old }()

	real := errors.New("up{job=\"hami-scheduler\"}=0")
	calls := 0
	err := waitUntil(context.Background(), 100*time.Millisecond, "x", func(string, ...any) {}, func(ctx context.Context) (bool, error) {
		calls++
		if calls == 1 {
			return false, real
		}
		<-ctx.Done() // 模拟一个卡到截止时间的请求
		return false, ctx.Err()
	})
	if err == nil || !errors.Is(err, real) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("got %v, want timeout carrying %v", err, real)
	}
}

func TestWaitUntilInterrupted(t *testing.T) {
	old := pollInterval
	pollInterval = 10 * time.Millisecond
	defer func() { pollInterval = old }()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	err := waitUntil(ctx, time.Minute, "x", func(string, ...any) {}, func(context.Context) (bool, error) { return false, nil })
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("got %v, want interrupted", err)
	}
}
