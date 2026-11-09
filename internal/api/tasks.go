// Package api 是 gputenant 的 REST API：开通和删除异步执行，状态以 Check 查到的实际结果为准。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zhexizx/gputenant/internal/tenant"
)

// 任务状态。任务只存在内存里：API 重启后旧任务全部消失（查询返回 404），
// 租户是否开通好只看 Check，不会因为重启前“任务成功”就报成功。
const (
	TaskRunning   = "running"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
)

// Task 是一次 create 或 delete。
type Task struct {
	ID       string      `json:"id"`
	Tenant   string      `json:"tenant"`
	Op       string      `json:"op"` // create 或 delete
	State    string      `json:"state"`
	Error    string      `json:"error,omitempty"`
	Started  time.Time   `json:"started"`
	Finished *time.Time  `json:"finished,omitempty"`
	Steps    []StepEvent `json:"steps"`
	Log      []string    `json:"log"`
}

// StepEvent 是 Reporter 收到的一条进度。
type StepEvent struct {
	Step  string       `json:"step"`
	Event tenant.Event `json:"event"`
	At    time.Time    `json:"at"`
}

// maxLogLines 限制每个任务保留的日志行数，内存不会随任务运行时间增长。
const maxLogLines = 200

// ErrBusy：这个租户已经有任务在跑。
var ErrBusy = errors.New("tenant already has a running task")

// TaskFunc 是任务的实际工作。
type TaskFunc func(ctx context.Context, report tenant.Reporter, logf tenant.Logf) error

// Tasks 管理内存里的任务。同一租户同时只能有一个任务。
type Tasks struct {
	ctx      context.Context // 服务退出时取消，正在跑的任务随之中断
	mu       sync.Mutex
	tasks    map[string]*Task
	active   map[string]string // tenant → task ID
	wg       sync.WaitGroup
	onFinish func(t Task, d time.Duration)
}

// NewTasks 创建任务管理器。onFinish 在任务结束时调用（记指标），可以为 nil。
func NewTasks(ctx context.Context, onFinish func(t Task, d time.Duration)) *Tasks {
	return &Tasks{ctx: ctx, tasks: map[string]*Task{}, active: map[string]string{}, onFinish: onFinish}
}

// Start 在后台启动任务。租户已有任务在跑时返回 ErrBusy 和那个任务。
func (m *Tasks) Start(tenantName, op string, fn TaskFunc) (Task, error) {
	m.mu.Lock()
	if id, ok := m.active[tenantName]; ok {
		t := m.snapshot(m.tasks[id])
		m.mu.Unlock()
		return t, ErrBusy
	}
	t := &Task{ID: newID(), Tenant: tenantName, Op: op, State: TaskRunning, Started: time.Now(), Steps: []StepEvent{}, Log: []string{}}
	m.tasks[t.ID] = t
	m.active[tenantName] = t.ID
	snap := m.snapshot(t)
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		report := func(step string, ev tenant.Event) {
			m.mu.Lock()
			t.Steps = append(t.Steps, StepEvent{Step: step, Event: ev, At: time.Now()})
			m.mu.Unlock()
		}
		logf := func(format string, args ...any) {
			line := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)
			m.mu.Lock()
			t.Log = append(t.Log, line)
			if len(t.Log) > maxLogLines {
				t.Log = t.Log[len(t.Log)-maxLogLines:]
			}
			m.mu.Unlock()
		}
		err := runSafely(m.ctx, fn, report, logf)

		m.mu.Lock()
		now := time.Now()
		t.Finished = &now
		if err != nil {
			t.State, t.Error = TaskFailed, err.Error()
		} else {
			t.State = TaskSucceeded
		}
		delete(m.active, tenantName)
		final := m.snapshot(t)
		m.mu.Unlock()
		if m.onFinish != nil {
			m.onFinish(final, now.Sub(final.Started))
		}
	}()
	return snap, nil
}

// runSafely 把任务里的 panic 变成失败，免得一个租户的 bug 拖垮整个服务、并让 active 永远占着。
func runSafely(ctx context.Context, fn TaskFunc, report tenant.Reporter, logf tenant.Logf) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn(ctx, report, logf)
}

// Get 返回任务的副本。
func (m *Tasks) Get(id string) (Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, false
	}
	return m.snapshot(t), true
}

// Active 返回租户正在跑的任务。
func (m *Tasks) Active(tenantName string) (Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.active[tenantName]
	if !ok {
		return Task{}, false
	}
	return m.snapshot(m.tasks[id]), true
}

// Running 返回正在跑的任务数。
func (m *Tasks) Running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

// Wait 等所有任务结束（服务退出时，ctx 取消之后调用）。
func (m *Tasks) Wait() { m.wg.Wait() }

// snapshot 复制任务，调用方要持有锁。返回给 HTTP 的是副本，序列化时不会和后台 goroutine 抢数据。
func (m *Tasks) snapshot(t *Task) Task {
	c := *t
	c.Steps = append([]StepEvent{}, t.Steps...)
	c.Log = append([]string{}, t.Log...)
	return c
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
