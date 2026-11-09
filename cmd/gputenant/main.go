// gputenant 管理多租户 GPU 平台上的租户：create、delete、status、verify。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhexizx/gputenant/internal/api"
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

// version 由构建时的 -ldflags 设置。
var version = "dev"

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newRootCmd() *cobra.Command {
	home, _ := os.UserHomeDir()
	g := &globalFlags{}
	root := &cobra.Command{
		Use:          "gputenant",
		Short:        "Provision GPU tenants on Kamaji + HAMi",
		Version:      version,
		SilenceUsage: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&g.kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "management cluster kubeconfig (default $KUBECONFIG)")
	pf.StringVar(&g.hamiChart, "hami-chart", envOr("GPUTENANT_HAMI_CHART", filepath.Join(home, "src/HAMi/charts/hami")),
		"HAMi chart directory checked out at the pinned commit (default $GPUTENANT_HAMI_CHART)")
	pf.StringVar(&g.docker, "docker", "docker", "docker binary")
	pf.StringVar(&g.helm, "helm", "helm", "helm binary")
	root.AddCommand(newCreateCmd(g), newDeleteCmd(g), newStatusCmd(g), newVerifyCmd(g), newServeCmd(g))
	return root
}

func (g *globalFlags) options() (tenant.Options, error) {
	return tenant.Options{Kubeconfig: g.kubeconfig, HAMiChart: g.hamiChart, DockerBin: g.docker, HelmBin: g.helm}.Complete()
}

// newEnv 校验租户名并连接管理集群。
func newEnv(g *globalFlags, cfg tenant.Config) (*tenant.Env, error) {
	o, err := g.options()
	if err != nil {
		return nil, err
	}
	return tenant.NewEnv(o, cfg, logf)
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
		Short: "Show per-step state, GPU allocation and firing alerts (read-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(g, tenant.Config{Tenant: args[0]})
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			start := time.Now()
			r := tenant.Inspect(ctx, e)
			for _, s := range r.Steps {
				fmt.Printf("  %-18s %-8s %s\n", s.Name, s.State, s.Error)
			}
			fmt.Printf("steps:  %d/%d done\n", r.StepsDone, r.StepsTotal)
			if r.GPU != nil {
				fmt.Printf("gpu:    %s, expected nvidia.com/gpu=%d\n", r.GPU, e.Cfg.ExpectedGPU())
			} else {
				fmt.Println("gpu:    nvml-mock not installed")
			}
			if u := r.Usage; u != nil {
				fmt.Printf("alloc:  vgpu %d/%d, memory %d/%d MiB, cores %d/%d%%\n",
					u.VGPUAllocated, u.VGPUCapacity, u.MemAllocMiB, u.MemCapacityMiB, u.CoresAllocated, u.CoresCapacity)
			}
			if r.Ready {
				if _, failed := r.Errors["alerts"]; !failed {
					if len(r.Alerts) == 0 {
						fmt.Println("alerts: none firing")
					} else {
						fmt.Printf("alerts: firing %s\n", strings.Join(r.Alerts, " "))
					}
				}
			}
			for k, v := range r.Errors {
				fmt.Printf("%s: unavailable: %s\n", k, v)
			}
			fmt.Printf("(%s)\n", time.Since(start).Round(100*time.Millisecond))
			switch {
			case !r.Ready:
				return fmt.Errorf("tenant %s not ready: %d/%d steps done", args[0], r.StepsDone, r.StepsTotal)
			case r.Degraded:
				return fmt.Errorf("tenant %s degraded: %s firing", args[0], strings.Join(tenant.StateAlertsIn(r.Alerts), " "))
			}
			return nil
		},
	}
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

func newServeCmd(g *globalFlags) *cobra.Command {
	var addr string
	var refresh time.Duration
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the REST API and /metrics",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			o, err := g.options()
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			return api.Serve(ctx, api.ClusterBackend{Opts: o}, addr, refresh)
		},
	}
	// 默认只监听本机：API 能开通和删除租户，镜像里还挂着 docker.sock，等同于主机 root。
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8080", "listen address")
	cmd.Flags().DurationVar(&refresh, "refresh", 30*time.Second, "how often tenant metrics are recomputed")
	return cmd
}
