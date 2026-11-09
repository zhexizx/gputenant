package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/zhexizx/gputenant/internal/tenant"
)

func init() { gin.SetMode(gin.TestMode) }

// fakeBackend：租户列表和 Inspect 结果固定；Create 阻塞到 release 关闭，模拟开通需要时间。
type fakeBackend struct {
	mu       sync.Mutex
	tenants  []string
	reports  map[string]tenant.Report
	release  chan struct{}
	createFn func(ctx context.Context) error
}

func newFake() *fakeBackend {
	return &fakeBackend{reports: map[string]tenant.Report{}, release: make(chan struct{})}
}

func (f *fakeBackend) List(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.tenants...), nil
}

func (f *fakeBackend) Inspect(_ context.Context, name string) (tenant.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.reports[name]; ok {
		return r, nil
	}
	return tenant.Report{Tenant: name, StepsTotal: 14}, nil
}

func (f *fakeBackend) Create(ctx context.Context, cfg tenant.Config, report tenant.Reporter, logf tenant.Logf) error {
	report("tenants-namespace", tenant.EventApplied)
	logf("creating %s", cfg.Tenant)
	if f.createFn != nil {
		return f.createFn(ctx)
	}
	select {
	case <-f.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeBackend) Delete(context.Context, string, tenant.Reporter, tenant.Logf) error { return nil }

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	tasks  *Tasks
	m      *Metrics
	cancel context.CancelFunc
}

func newHarness(t *testing.T, b Backend) *harness {
	ctx, cancel := context.WithCancel(context.Background())
	m := NewMetrics(b)
	tasks := NewTasks(ctx, m.TaskFinished)
	m.RegisterTasks(tasks)
	srv := httptest.NewServer(NewServer(b, tasks, m).Handler())
	h := &harness{t: t, srv: srv, tasks: tasks, m: m, cancel: cancel}
	t.Cleanup(func() { srv.Close(); cancel(); tasks.Wait() })
	return h
}

func (h *harness) do(method, path, body string) (int, map[string]any, http.Header) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, resp.Header
}

// waitTask 轮询直到任务结束。
func (h *harness) waitTask(id string) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, t, _ := h.do("GET", "/tasks/"+id, "")
		if t["state"] != TaskRunning {
			return t
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("task %s did not finish", id)
	return nil
}

const createA = `{"name":"t1","gpuProfile":"t4","gpuCount":2}`

func TestCreateReturns202ThenConflict(t *testing.T) {
	f := newFake()
	h := newHarness(t, f)

	code, task, hdr := h.do("POST", "/tenants", createA)
	if code != http.StatusAccepted || task["state"] != TaskRunning {
		t.Fatalf("POST: %d %v", code, task)
	}
	id := task["id"].(string)
	if hdr.Get("Location") != "/tasks/"+id {
		t.Errorf("Location = %q", hdr.Get("Location"))
	}
	// 同一租户的第二个请求（create 或 delete）都要 409，并指向正在跑的任务。
	for _, req := range [][2]string{{"POST", "/tenants"}, {"DELETE", "/tenants/t1"}} {
		code, body, _ := h.do(req[0], req[1], createA)
		if code != http.StatusConflict {
			t.Fatalf("%s while running: %d %v", req[0], code, body)
		}
		if body["task"].(map[string]any)["id"] != id {
			t.Errorf("409 body does not point at running task: %v", body)
		}
	}
	// 别的租户不受影响。
	if code, body, _ := h.do("POST", "/tenants", `{"name":"t2","gpuProfile":"a100","gpuCount":1}`); code != http.StatusAccepted {
		t.Fatalf("other tenant: %d %v", code, body)
	}

	close(f.release)
	done := h.waitTask(id)
	if done["state"] != TaskSucceeded || done["finished"] == nil {
		t.Fatalf("task: %v", done)
	}
	if steps := done["steps"].([]any); len(steps) != 1 {
		t.Errorf("steps not recorded: %v", steps)
	}
	// 任务结束后可以再发。
	if code, _, _ := h.do("DELETE", "/tenants/t1", ""); code != http.StatusAccepted {
		t.Fatalf("DELETE after finish: %d", code)
	}
}

func TestCreateValidation(t *testing.T) {
	h := newHarness(t, newFake())
	for _, body := range []string{
		`not json`,
		`{"name":"Bad_Name","gpuProfile":"t4","gpuCount":1}`,
		`{"name":"t1","gpuProfile":"h100","gpuCount":1}`,
		`{"name":"t1","gpuProfile":"t4","gpuCount":0}`,
	} {
		if code, _, _ := h.do("POST", "/tenants", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, code)
		}
	}
	if h.tasks.Running() != 0 {
		t.Fatal("invalid request started a task")
	}
}

func TestFailedTaskReportsError(t *testing.T) {
	f := newFake()
	f.createFn = func(context.Context) error { return errors.New("calico: apply: boom") }
	h := newHarness(t, f)
	_, task, _ := h.do("POST", "/tenants", createA)
	done := h.waitTask(task["id"].(string))
	if done["state"] != TaskFailed || !strings.Contains(done["error"].(string), "boom") {
		t.Fatalf("task: %v", done)
	}
	if v := testutil.ToFloat64(h.m.provisionTotal.WithLabelValues("create", "failure")); v != 1 {
		t.Errorf("failure counter = %v", v)
	}
}

func TestPanicInTaskBecomesFailure(t *testing.T) {
	f := newFake()
	f.createFn = func(context.Context) error { panic("nil map") }
	h := newHarness(t, f)
	_, task, _ := h.do("POST", "/tenants", createA)
	done := h.waitTask(task["id"].(string))
	if done["state"] != TaskFailed {
		t.Fatalf("task: %v", done)
	}
	// 租户不能被永远占住。
	if code, _, _ := h.do("POST", "/tenants", createA); code != http.StatusAccepted {
		t.Fatalf("tenant still locked after panic: %d", code)
	}
}

// API 重启后：旧任务查不到（404），租户状态只看 Inspect，不会因为重启前任务成功就报 ready。
func TestRestartDoesNotReportStaleSuccess(t *testing.T) {
	f := newFake()
	close(f.release)
	h1 := newHarness(t, f)
	_, task, _ := h1.do("POST", "/tenants", createA)
	id := task["id"].(string)
	if done := h1.waitTask(id); done["state"] != TaskSucceeded {
		t.Fatalf("task: %v", done)
	}

	// 实际集群里租户只做完了一半。
	f.mu.Lock()
	f.tenants = []string{"t1"}
	f.reports["t1"] = tenant.Report{Tenant: "t1", StepsDone: 9, StepsTotal: 14}
	f.mu.Unlock()

	h2 := newHarness(t, f) // 新进程：任务表是空的
	if code, body, _ := h2.do("GET", "/tasks/"+id, ""); code != http.StatusNotFound {
		t.Fatalf("old task after restart: %d %v", code, body)
	}
	code, rep, _ := h2.do("GET", "/tenants/t1", "")
	if code != http.StatusOK || rep["ready"] != false || rep["stepsDone"] != float64(9) {
		t.Fatalf("tenant after restart: %d %v", code, rep)
	}
	h2.m.Refresh(context.Background())
	if v := testutil.ToFloat64(collectGauge(t, h2.m, "gputenant_tenant_ready", "t1")); v != 0 {
		t.Fatalf("gputenant_tenant_ready = %v after restart, want 0", v)
	}
}

func TestGetTenantNotFound(t *testing.T) {
	h := newHarness(t, newFake())
	if code, _, _ := h.do("GET", "/tenants/nope", ""); code != http.StatusNotFound {
		t.Fatalf("GET missing tenant: %d", code)
	}
}

// 正在开通的租户还没有 TCP，也要能查状态。
func TestGetTenantWhileCreating(t *testing.T) {
	f := newFake()
	h := newHarness(t, f)
	h.do("POST", "/tenants", createA)
	code, rep, _ := h.do("GET", "/tenants/t1", "")
	if code != http.StatusOK || rep["activeTask"] == nil {
		t.Fatalf("GET creating tenant: %d %v", code, rep)
	}
	close(f.release)
}
