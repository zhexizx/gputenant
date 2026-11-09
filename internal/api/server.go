package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zhexizx/gputenant/internal/tenant"
)

// Backend 是 API 背后的实际操作，测试时换成假的。
type Backend interface {
	List(ctx context.Context) ([]string, error)
	Inspect(ctx context.Context, name string) (tenant.Report, error)
	Create(ctx context.Context, cfg tenant.Config, report tenant.Reporter, logf tenant.Logf) error
	Delete(ctx context.Context, name string, report tenant.Reporter, logf tenant.Logf) error
}

// Server 把 Backend、任务和指标接到 HTTP 上。
type Server struct {
	backend Backend
	tasks   *Tasks
	metrics *Metrics
}

func NewServer(b Backend, t *Tasks, m *Metrics) *Server {
	return &Server{backend: b, tasks: t, metrics: m}
}

// Handler 返回路由。
func (s *Server) Handler() http.Handler {
	r := gin.New()
	r.Use(gin.Recovery(), gin.Logger())
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/tenants", s.listTenants)
	r.GET("/tenants/:name", s.getTenant)
	r.POST("/tenants", s.createTenant)
	r.DELETE("/tenants/:name", s.deleteTenant)
	r.GET("/tasks/:id", s.getTask)
	if s.metrics != nil {
		r.GET("/metrics", gin.WrapH(s.metrics.Handler()))
	}
	return r
}

type apiError struct {
	Error string `json:"error"`
	Task  *Task  `json:"task,omitempty"`
}

func fail(c *gin.Context, code int, err error) {
	c.JSON(code, apiError{Error: err.Error()})
}

// tenantSummary 是列表里的一项。完整状态要查 GET /tenants/:name，那里会跑全部 Check。
type tenantSummary struct {
	Tenant     string `json:"tenant"`
	ActiveTask *Task  `json:"activeTask,omitempty"`
}

func (s *Server) listTenants(c *gin.Context) {
	names, err := s.backend.List(c.Request.Context())
	if err != nil {
		fail(c, http.StatusBadGateway, fmt.Errorf("list tenants: %w", err))
		return
	}
	out := []tenantSummary{}
	for _, n := range names {
		out = append(out, s.summary(n))
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) summary(name string) tenantSummary {
	ts := tenantSummary{Tenant: name}
	if t, ok := s.tasks.Active(name); ok {
		ts.ActiveTask = &t
	}
	return ts
}

type tenantResponse struct {
	tenant.Report
	ActiveTask *Task `json:"activeTask,omitempty"`
}

// getTenant 每次都重新跑 Check：状态来自集群，不来自内存里的任务记录。
func (s *Server) getTenant(c *gin.Context) {
	name := c.Param("name")
	if err := tenant.ValidateName(name); err != nil {
		fail(c, http.StatusBadRequest, err)
		return
	}
	ctx := c.Request.Context()
	active, busy := s.tasks.Active(name)
	if !busy {
		names, err := s.backend.List(ctx)
		if err != nil {
			fail(c, http.StatusBadGateway, fmt.Errorf("list tenants: %w", err))
			return
		}
		if !slices.Contains(names, name) {
			fail(c, http.StatusNotFound, fmt.Errorf("tenant %s not found", name))
			return
		}
	}
	rep, err := s.backend.Inspect(ctx, name)
	if err != nil {
		fail(c, http.StatusBadGateway, err)
		return
	}
	resp := tenantResponse{Report: rep}
	if busy {
		resp.ActiveTask = &active
	}
	c.JSON(http.StatusOK, resp)
}

type createRequest struct {
	Name       string `json:"name"`
	GPUProfile string `json:"gpuProfile"`
	GPUCount   int    `json:"gpuCount"`
}

func (s *Server) createTenant(c *gin.Context) {
	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	cfg := tenant.Config{Tenant: req.Name, GPUProfile: req.GPUProfile, GPUCount: req.GPUCount}
	if err := tenant.ValidateName(cfg.Tenant); err != nil {
		fail(c, http.StatusBadRequest, err)
		return
	}
	if err := cfg.ValidateGPU(); err != nil {
		fail(c, http.StatusBadRequest, err)
		return
	}
	s.start(c, cfg.Tenant, "create", func(ctx context.Context, report tenant.Reporter, logf tenant.Logf) error {
		return s.backend.Create(ctx, cfg, report, logf)
	})
}

func (s *Server) deleteTenant(c *gin.Context) {
	name := c.Param("name")
	if err := tenant.ValidateName(name); err != nil {
		fail(c, http.StatusBadRequest, err)
		return
	}
	s.start(c, name, "delete", func(ctx context.Context, report tenant.Reporter, logf tenant.Logf) error {
		return s.backend.Delete(ctx, name, report, logf)
	})
}

// start 启动任务并返回 202；租户已有任务在跑时返回 409 和那个任务。
func (s *Server) start(c *gin.Context, name, op string, fn TaskFunc) {
	t, err := s.tasks.Start(name, op, fn)
	if errors.Is(err, ErrBusy) {
		c.JSON(http.StatusConflict, apiError{Error: fmt.Sprintf("tenant %s already has a running %s task", name, t.Op), Task: &t})
		return
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, err)
		return
	}
	c.Header("Location", "/tasks/"+t.ID)
	c.JSON(http.StatusAccepted, t)
}

func (s *Server) getTask(c *gin.Context) {
	t, ok := s.tasks.Get(c.Param("id"))
	if !ok {
		fail(c, http.StatusNotFound, errors.New("task not found: tasks live in memory and are lost when the API restarts; GET /tenants/<name> shows the actual state"))
		return
	}
	c.JSON(http.StatusOK, t)
}

// Serve 启动 HTTP 服务和指标刷新，ctx 取消时优雅退出：停止接收请求，中断正在跑的任务并等它们结束。
// 被中断的任务没有完成的步骤，下次 create 会从那里接着做。
func Serve(ctx context.Context, b Backend, addr string, refresh time.Duration) error {
	gin.SetMode(gin.ReleaseMode)
	m := NewMetrics(b)
	tasks := NewTasks(ctx, m.TaskFinished)
	m.RegisterTasks(tasks)
	go m.Run(ctx, refresh)

	srv := &http.Server{Addr: addr, Handler: NewServer(b, tasks, m).Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("gputenant API listening on %s", addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Printf("shutting down: waiting for %d running task(s) to stop", tasks.Running())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	tasks.Wait()
	return nil
}
