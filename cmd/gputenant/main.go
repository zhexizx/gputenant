// gputenant 管理多租户 GPU 平台上的租户：create、delete、status、verify。
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var errNotImplemented = errors.New("not implemented yet")

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "gputenant",
		Short:         "Provision GPU tenants on Kamaji + HAMi",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(newCreateCmd(), newDeleteCmd(), newStatusCmd(), newVerifyCmd())
	return root
}

func newCreateCmd() *cobra.Command {
	var gpuProfile string
	var gpuCount int
	cmd := &cobra.Command{
		Use:   "create <tenant>",
		Short: "Create a tenant (re-runnable: finished steps are skipped)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if gpuProfile != "a100" && gpuProfile != "t4" {
				return fmt.Errorf("--gpu-profile must be a100 or t4, got %q", gpuProfile)
			}
			if gpuCount < 1 {
				return fmt.Errorf("--gpu-count must be >= 1, got %d", gpuCount)
			}
			return errNotImplemented
		},
	}
	cmd.Flags().StringVar(&gpuProfile, "gpu-profile", "t4", "mock GPU profile: a100 or t4")
	cmd.Flags().IntVar(&gpuCount, "gpu-count", 2, "number of mock GPUs on the tenant worker")
	return cmd
}

func newDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <tenant>",
		Short: "Delete a tenant, tolerating partially created state",
		Args:  cobra.ExactArgs(1),
		RunE:  func(*cobra.Command, []string) error { return errNotImplemented },
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
