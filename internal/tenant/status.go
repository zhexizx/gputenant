package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// checkTimeout 限制单个 Check 的耗时，status 要几秒内返回，某一步卡住不能拖住整个命令。
const checkTimeout = 10 * time.Second

// StepState 是一步的 Check 结果。
type StepState struct {
	Name string
	Done bool
	// Err 是 Check 的错误，只在前面的步骤都已完成时保留。
	Err error
	// Blocked：前面有步骤没完成，这一步的 Check 报错多半只是因为依赖还不存在（例如还没有 kubeconfig）。
	Blocked bool
}

// Status 依次执行每一步的 Check，不在第一个未完成的步骤停下，方便看出缺了哪几步。只读。
func Status(ctx context.Context, steps []Step) []StepState {
	states := make([]StepState, 0, len(steps))
	allDone := true
	for _, s := range steps {
		cctx, cancel := context.WithTimeout(ctx, checkTimeout)
		done, err := s.Check(cctx)
		cancel()
		st := StepState{Name: s.Name(), Done: done && err == nil}
		if err != nil {
			if allDone {
				st.Err = err
			} else {
				st.Blocked = true
			}
		}
		allDone = allDone && st.Done
		states = append(states, st)
	}
	return states
}

// GPU 是租户 nvml-mock 的配置。
type GPU struct {
	Profile string
	Count   int
}

func (g GPU) String() string { return fmt.Sprintf("%s×%d", g.Profile, g.Count) }

var errNoRelease = errors.New("nvml-mock release not found")

// InstalledGPU 从租户里 nvml-mock 的 Helm release 读回 GPU 配置。
// status、verify、delete 没有 --gpu-* 参数，期望的 GPU 数只能从这里来。没装时返回 errNoRelease。
func (e *Env) InstalledGPU(ctx context.Context) (GPU, error) {
	if _, err := os.Stat(e.Cfg.Kubeconfig()); err != nil {
		return GPU{}, errNoRelease
	}
	out, err := e.Helm.Run(ctx, "get", "values", "nvml-mock", "-n", "default", "--kubeconfig", e.Cfg.Kubeconfig(), "-o", "json")
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return GPU{}, errNoRelease
		}
		return GPU{}, err
	}
	return parseGPUValues(out)
}

func parseGPUValues(out string) (GPU, error) {
	var v struct {
		GPU struct {
			Profile string          `json:"profile"`
			Count   json.RawMessage `json:"count"`
		} `json:"gpu"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return GPU{}, fmt.Errorf("nvml-mock values: %w", err)
	}
	// --set gpu.count=2 存成数字，--set-string 会存成字符串，两种都接受。
	n, err := strconv.Atoi(strings.Trim(string(v.GPU.Count), `"`))
	if err != nil || v.GPU.Profile == "" {
		return GPU{}, fmt.Errorf("nvml-mock values lack gpu.profile/gpu.count: %s", out)
	}
	return GPU{Profile: v.GPU.Profile, Count: n}, nil
}

// LoadGPU 把已安装的 GPU 配置填进 Cfg。还没装 nvml-mock 时保持为空，hami 的 Check 会报未完成。
func (e *Env) LoadGPU(ctx context.Context) error {
	g, err := e.InstalledGPU(ctx)
	if errors.Is(err, errNoRelease) {
		return nil
	}
	if err != nil {
		return err
	}
	e.Cfg.GPUProfile, e.Cfg.GPUCount = g.Profile, g.Count
	return nil
}

// CheckGPUMatches 防止对已有租户用不同的 GPU 参数重跑 create：
// nvml-mock 的 Check 不比较参数，结果会在 hami 那一步干等 GPU 容量变成新值。
func (e *Env) CheckGPUMatches(ctx context.Context) error {
	g, err := e.InstalledGPU(ctx)
	if errors.Is(err, errNoRelease) {
		return nil
	}
	if err != nil {
		return err
	}
	want := GPU{Profile: e.Cfg.GPUProfile, Count: e.Cfg.GPUCount}
	if g != want {
		return fmt.Errorf("tenant %s already has GPU %s, requested %s: pass matching --gpu-profile/--gpu-count or delete it first", e.Cfg.Tenant, g, want)
	}
	return nil
}

// NodeGPU 是工作节点上的 nvidia.com/gpu 数量。
type NodeGPU struct {
	Capacity, Allocatable int64
}

// WorkerGPU 读取工作节点的 GPU 容量。
func (e *Env) WorkerGPU(ctx context.Context) (NodeGPU, error) {
	k, err := e.TenantKube()
	if err != nil {
		return NodeGPU{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	n, err := k.Clientset.CoreV1().Nodes().Get(ctx, e.Cfg.Worker(), metav1.GetOptions{})
	if err != nil {
		return NodeGPU{}, err
	}
	c := n.Status.Capacity["nvidia.com/gpu"]
	a := n.Status.Allocatable["nvidia.com/gpu"]
	return NodeGPU{Capacity: c.Value(), Allocatable: a.Value()}, nil
}

// FiringAlertsQuick 是带超时的 FiringAlerts，给 status 用。
func (e *Env) FiringAlertsQuick(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	return FiringAlerts(ctx, e)
}
