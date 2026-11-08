// gputenant 管理多租户 GPU 平台上的租户：create、delete、status、verify。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhexizx/gputenant/internal/tenant"
)

var errNotImplemented = errors.New("not implemented yet")

// globalFlags 是所有子命令共用的参数。
type globalFlags struct {
	kubeconfig string // 管理集群
	hamiChart  string
	docker     string
	helm       string
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	home, _ := os.UserHomeDir()
	g := &globalFlags{}
	root := &cobra.Command{
		Use:          "gputenant",
		Short:        "Provision GPU tenants on Kamaji + HAMi",
		SilenceUsage: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&g.kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "management cluster kubeconfig (default $KUBECONFIG)")
	pf.StringVar(&g.hamiChart, "hami-chart", filepath.Join(home, "src/HAMi/charts/hami"), "HAMi chart directory checked out at the pinned commit")
	pf.StringVar(&g.docker, "docker", "docker", "docker binary")
	pf.StringVar(&g.helm, "helm", "helm", "helm binary")
	root.AddCommand(newCreateCmd(g), newDeleteCmd(g), newStatusCmd(), newVerifyCmd())
	return root
}

// newEnv 校验租户名并连接管理集群。
func newEnv(g *globalFlags, cfg tenant.Config) (*tenant.Env, error) {
	if err := tenant.ValidateName(cfg.Tenant); err != nil {
		return nil, err
	}
	if g.kubeconfig == "" {
		return nil, errors.New("management cluster kubeconfig not set: pass --kubeconfig or set KUBECONFIG")
	}
	home, err := tenant.DefaultHome()
	if err != nil {
		return nil, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	cfg.Home = home
	cfg.CacheDir = filepath.Join(cache, "gputenant")
	cfg.HAMiChart = g.hamiChart
	mgmt, err := tenant.NewKube(g.kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("management kubeconfig: %w", err)
	}
	return &tenant.Env{
		Cfg:    cfg,
		Docker: tenant.CLIDocker{Bin: g.docker},
		Helm:   tenant.CLIHelm{Bin: g.helm},
		Mgmt:   mgmt,
		Log:    logf,
	}, nil
}

func logf(format string, args ...any) {
	fmt.Printf("[%s]   %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func report(step string, ev tenant.Event) {
	fmt.Printf("[%s] %-18s %s\n", time.Now().Format("15:04:05"), step, ev)
}

// signalContext 在 Ctrl-C 时取消，让 kill 后重跑的路径和正常失败一样。
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func newCreateCmd(g *globalFlags) *cobra.Command {
	cfg := tenant.Config{}
	cmd := &cobra.Command{
		Use:   "create <tenant>",
		Short: "Create a tenant (re-runnable: finished steps are skipped)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg.Tenant = args[0]
			if err := cfg.ValidateGPU(); err != nil {
				return err
			}
			e, err := newEnv(g, cfg)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			if err := e.PreflightCreate(ctx); err != nil {
				return fmt.Errorf("preflight: %w", err)
			}
			start := time.Now()
			if err := tenant.Create(ctx, tenant.Steps(e), report); err != nil {
				return err
			}
			fmt.Printf("tenant %s ready in %s\nkubeconfig: %s\n", cfg.Tenant, time.Since(start).Round(time.Second), e.Cfg.Kubeconfig())
			return nil
		},
	}
	cmd.Flags().StringVar(&cfg.GPUProfile, "gpu-profile", "t4", "mock GPU profile: a100 or t4")
	cmd.Flags().IntVar(&cfg.GPUCount, "gpu-count", 2, "number of mock GPUs on the tenant worker")
	return cmd
}

func newDeleteCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <tenant>",
		Short: "Delete a tenant, tolerating partially created state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(g, tenant.Config{Tenant: args[0]})
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			if err := e.PreflightDelete(ctx); err != nil {
				return fmt.Errorf("preflight: %w", err)
			}
			if err := tenant.Delete(ctx, tenant.Steps(e), report); err != nil {
				return err
			}
			fmt.Printf("tenant %s deleted\n", args[0])
			return nil
		},
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <tenant>",
		Short: "Show per-step state, GPU capacity and firing alerts (read-only)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(*cobra.Command, []string) error { return errNotImplemented },
	}
}

func newVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify <tenant>",
		Short: "Smoke-test a tenant",
		Args:  cobra.ExactArgs(1),
		RunE:  func(*cobra.Command, []string) error { return errNotImplemented },
	}
}
