// gputenant 管理多租户 GPU 平台上的租户：create、delete、status、verify。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhexizx/gputenant/internal/tenant"
)

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
	root.AddCommand(newCreateCmd(g), newDeleteCmd(g), newStatusCmd(g), newVerifyCmd(g))
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
			if err := e.CheckGPUMatches(ctx); err != nil {
				return err
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

func newStatusCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status <tenant>",
		Short: "Show per-step state, GPU capacity and firing alerts (read-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(g, tenant.Config{Tenant: args[0]})
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			start := time.Now()
			if err := e.LoadGPU(ctx); err != nil {
				fmt.Printf("gpu config: %v\n", err)
			}
			states := tenant.Status(ctx, tenant.Steps(e))
			done := 0
			for _, s := range states {
				switch {
				case s.Err != nil:
					fmt.Printf("  %-18s error    %v\n", s.Name, s.Err)
				case s.Done:
					done++
					fmt.Printf("  %-18s done\n", s.Name)
				default:
					fmt.Printf("  %-18s pending\n", s.Name)
				}
			}
			fmt.Printf("steps:  %d/%d done\n", done, len(states))
			if e.Cfg.GPUCount > 0 {
				fmt.Printf("gpu:    %s, expected nvidia.com/gpu=%d\n", tenant.GPU{Profile: e.Cfg.GPUProfile, Count: e.Cfg.GPUCount}, e.Cfg.ExpectedGPU())
			} else {
				fmt.Println("gpu:    nvml-mock not installed")
			}
			if ng, err := e.WorkerGPU(ctx); err != nil {
				fmt.Printf("node:   unavailable: %v\n", shortErr(err))
			} else {
				fmt.Printf("node:   nvidia.com/gpu capacity=%d allocatable=%d\n", ng.Capacity, ng.Allocatable)
			}
			alerts, alertErr := e.FiringAlertsQuick(ctx)
			if err := alertErr; err != nil {
				fmt.Printf("alerts: unavailable: %v\n", shortErr(err))
			} else if len(alerts) == 0 {
				fmt.Println("alerts: none firing")
			} else {
				fmt.Printf("alerts: firing %s\n", strings.Join(alerts, " "))
			}
			fmt.Printf("(%s)\n", time.Since(start).Round(100*time.Millisecond))
			if done != len(states) {
				return fmt.Errorf("tenant %s not ready: %d/%d steps done", args[0], done, len(states))
			}
			// 步骤都完成不代表现在是好的：调度器或隧道挂掉时 Check 照样为 true，要看告警。
			if bad := tenant.StateAlertsIn(alerts); len(bad) > 0 {
				return fmt.Errorf("tenant %s degraded: %s firing", args[0], strings.Join(bad, " "))
			}
			return nil
		},
	}
}

// shortErr 把“没有 kubeconfig”这种常见情况说短一点。
func shortErr(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "tenant kubeconfig not found"
	}
	return err.Error()
}

func newVerifyCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "verify <tenant>",
		Short: "Smoke-test a tenant (restarts kube-proxy and runs a short-lived GPU Pod)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(g, tenant.Config{Tenant: args[0]})
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			if err := e.LoadGPU(ctx); err != nil {
				return err
			}
			start := time.Now()
			failed := tenant.Verify(ctx, tenant.VerifyChecks(e), func(name string, err error) {
				if err != nil {
					fmt.Printf("[%s] FAIL %s: %v\n", time.Now().Format("15:04:05"), name, err)
				} else {
					fmt.Printf("[%s] ok   %s\n", time.Now().Format("15:04:05"), name)
				}
			})
			if ctx.Err() != nil {
				return fmt.Errorf("interrupted")
			}
			if failed > 0 {
				return fmt.Errorf("tenant %s: %d check(s) failed", args[0], failed)
			}
			fmt.Printf("tenant %s verified in %s\n", args[0], time.Since(start).Round(time.Second))
			return nil
		},
	}
}
