package api

import (
	"context"
	"fmt"

	"github.com/zhexizx/gputenant/internal/tenant"
)

// ClusterBackend 用真实的管理集群、Docker 和 Helm 执行操作，和 CLI 走同一套代码。
type ClusterBackend struct {
	Opts tenant.Options // 已 Complete
}

func (b ClusterBackend) env(name string, cfg tenant.Config, logf tenant.Logf) (*tenant.Env, error) {
	cfg.Tenant = name
	return tenant.NewEnv(b.Opts, cfg, logf)
}

func (b ClusterBackend) List(ctx context.Context) ([]string, error) {
	mgmt, err := tenant.NewKube(b.Opts.Kubeconfig)
	if err != nil {
		return nil, err
	}
	return tenant.ListTenants(ctx, mgmt)
}

func (b ClusterBackend) Inspect(ctx context.Context, name string) (tenant.Report, error) {
	e, err := b.env(name, tenant.Config{}, nil)
	if err != nil {
		return tenant.Report{}, err
	}
	return tenant.Inspect(ctx, e), nil
}

func (b ClusterBackend) Create(ctx context.Context, cfg tenant.Config, report tenant.Reporter, logf tenant.Logf) error {
	e, err := b.env(cfg.Tenant, cfg, logf)
	if err != nil {
		return err
	}
	if err := e.PreflightCreate(ctx); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if err := e.CheckGPUMatches(ctx); err != nil {
		return err
	}
	return tenant.Create(ctx, tenant.Steps(e), report)
}

func (b ClusterBackend) Delete(ctx context.Context, name string, report tenant.Reporter, logf tenant.Logf) error {
	e, err := b.env(name, tenant.Config{}, logf)
	if err != nil {
		return err
	}
	if err := e.PreflightDelete(ctx); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	return tenant.Delete(ctx, tenant.Steps(e), report)
}
