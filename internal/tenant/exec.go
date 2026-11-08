package tenant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Docker 封装 docker 命令行，方便测试时替换。
type Docker interface {
	// Run 执行 docker <args>，返回去掉首尾空白的 stdout。失败时错误里带 stderr。
	Run(ctx context.Context, stdin io.Reader, args ...string) (string, error)
	// SaveToNode 把本地镜像导入节点容器的 containerd（docker save | ctr import）。
	SaveToNode(ctx context.Context, node string, images ...string) error
}

// Helm 封装 helm 命令行。
type Helm interface {
	Run(ctx context.Context, args ...string) (string, error)
}

func runCmd(ctx context.Context, stdin io.Reader, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", bin, firstArg(args), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// firstArg 只把子命令放进错误信息，避免把 join 命令之类的敏感参数打出来。
func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

type CLIDocker struct{ Bin string }

func (d CLIDocker) Run(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	return runCmd(ctx, stdin, d.Bin, args...)
}

func (d CLIDocker) SaveToNode(ctx context.Context, node string, images ...string) error {
	save := exec.CommandContext(ctx, d.Bin, append([]string{"save"}, images...)...)
	pipe, err := save.StdoutPipe()
	if err != nil {
		return err
	}
	var saveErr bytes.Buffer
	save.Stderr = &saveErr
	if err := save.Start(); err != nil {
		return err
	}
	_, importErr := d.Run(ctx, pipe, "exec", "-i", node, "ctr", "-n", "k8s.io", "images", "import", "-")
	if err := save.Wait(); err != nil {
		return fmt.Errorf("docker save: %w: %s", err, strings.TrimSpace(saveErr.String()))
	}
	return importErr
}

type CLIHelm struct{ Bin string }

func (h CLIHelm) Run(ctx context.Context, args ...string) (string, error) {
	return runCmd(ctx, nil, h.Bin, args...)
}

// containerState 返回容器状态（running、exited 等），容器不存在时返回 ""。
func containerState(ctx context.Context, d Docker, name string) (string, error) {
	return d.Run(ctx, nil, "ps", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.State}}")
}

// removeContainer 删除容器和匿名卷，不存在时不报错。
func removeContainer(ctx context.Context, d Docker, name string) error {
	state, err := containerState(ctx, d, name)
	if err != nil || state == "" {
		return err
	}
	_, err = d.Run(ctx, nil, "rm", "-fv", name)
	return err
}

// helmDeployed 报告 release 是否处于 deployed 状态。
func helmDeployed(ctx context.Context, h Helm, kubeconfig, ns, release string) (bool, error) {
	out, err := h.Run(ctx, "status", release, "-n", ns, "--kubeconfig", kubeconfig, "-o", "json")
	if err != nil {
		if strings.Contains(err.Error(), "release: not found") {
			return false, nil
		}
		return false, err
	}
	var st struct {
		Info struct {
			Status string `json:"status"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return false, fmt.Errorf("helm status %s: %w", release, err)
	}
	return st.Info.Status == "deployed", nil
}
