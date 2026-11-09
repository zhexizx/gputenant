package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Report 是一个租户当前的实际状态，全部来自查询集群和 Docker，CLI 的 status 和 API 共用。
type Report struct {
	Tenant     string       `json:"tenant"`
	Ready      bool         `json:"ready"`    // 所有步骤完成
	Degraded   bool         `json:"degraded"` // 步骤完成，但有状态告警 firing
	StepsDone  int          `json:"stepsDone"`
	StepsTotal int          `json:"stepsTotal"`
	Steps      []StepReport `json:"steps"`
	GPU        *GPU         `json:"gpu,omitempty"` // nvml-mock 配置，没装时为空
	Usage      *GPUUsage    `json:"usage,omitempty"`
	Alerts     []string     `json:"firingAlerts"`
	// Errors 记录读不到的部分（节点、告警等），不影响 Ready 的判断。
	Errors map[string]string `json:"errors,omitempty"`
}

// StepReport 是一步的状态：done、pending 或 error。
type StepReport struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// GPUUsage 是工作节点的 GPU 容量和 HAMi 已分配的量。这是分配率，不是利用率。
type GPUUsage struct {
	VGPUCapacity   int64 `json:"vgpuCapacity"` // nvidia.com/gpu：卡数 × 切分份数
	VGPUAllocated  int64 `json:"vgpuAllocated"`
	MemCapacityMiB int64 `json:"memCapacityMiB"`
	MemAllocMiB    int64 `json:"memAllocatedMiB"`
	CoresCapacity  int64 `json:"coresCapacity"` // 每张卡 100
	CoresAllocated int64 `json:"coresAllocated"`
}

// Inspect 汇总租户状态。只读；某一部分读不到时记进 Errors，继续读其他部分。
func Inspect(ctx context.Context, e *Env) Report {
	r := Report{Tenant: e.Cfg.Tenant, Alerts: []string{}, Errors: map[string]string{}}
	if err := e.LoadGPU(ctx); err != nil {
		r.Errors["gpu"] = err.Error()
	}
	if e.Cfg.GPUCount > 0 {
		r.GPU = &GPU{Profile: e.Cfg.GPUProfile, Count: e.Cfg.GPUCount}
	}
	for _, s := range Status(ctx, Steps(e)) {
		sr := StepReport{Name: s.Name, State: "pending"}
		switch {
		case s.Err != nil:
			sr.State, sr.Error = "error", s.Err.Error()
		case s.Done:
			sr.State = "done"
			r.StepsDone++
		}
		r.Steps = append(r.Steps, sr)
	}
	r.StepsTotal = len(r.Steps)
	r.Ready = r.StepsDone == r.StepsTotal
	if !r.Ready {
		// 没开通完时节点和 Prometheus 多半还不存在，不去读，免得 Errors 里全是意料之中的错误。
		return r
	}
	if u, err := e.GPUUsage(ctx); err != nil {
		r.Errors["usage"] = err.Error()
	} else {
		r.Usage = &u
	}
	if alerts, err := e.FiringAlertsQuick(ctx); err != nil {
		r.Errors["alerts"] = err.Error()
	} else {
		if alerts != nil {
			r.Alerts = alerts
		}
		// 步骤都完成不代表现在是好的：调度器或隧道挂掉时 Check 照样为 true。
		r.Degraded = len(StateAlertsIn(alerts)) > 0
	}
	return r
}

// GPUUsage 读节点上 HAMi 登记的设备和 Pod 上的分配注解。
func (e *Env) GPUUsage(ctx context.Context) (GPUUsage, error) {
	k, err := e.TenantKube()
	if err != nil {
		return GPUUsage{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	n, err := k.Clientset.CoreV1().Nodes().Get(ctx, e.Cfg.Worker(), metav1.GetOptions{})
	if err != nil {
		return GPUUsage{}, err
	}
	u, err := nodeGPUCapacity(n.Annotations[nodeRegisterAnnotation])
	if err != nil {
		return GPUUsage{}, err
	}
	pods, err := k.Clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + e.Cfg.Worker()})
	if err != nil {
		return GPUUsage{}, err
	}
	for _, p := range pods.Items {
		if !holdsGPU(&p) {
			continue
		}
		for _, d := range parseAllocated(p.Annotations[allocatedAnnotation]) {
			u.VGPUAllocated++
			u.MemAllocMiB += d.MemMiB
			u.CoresAllocated += d.Cores
		}
	}
	return u, nil
}

const (
	nodeRegisterAnnotation = "hami.io/node-nvidia-register"
	allocatedAnnotation    = "hami.io/vgpu-devices-allocated"
)

// holdsGPU：Pod 还占着分配的资源。已结束或正在删除的不算。
func holdsGPU(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	return p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed
}

// nodeGPUCapacity 解析 hami.io/node-nvidia-register：每张卡的切分份数、显存（MiB）、算力。
func nodeGPUCapacity(ann string) (GPUUsage, error) {
	if ann == "" {
		return GPUUsage{}, errors.New("node has no " + nodeRegisterAnnotation + " annotation (HAMi device plugin not registered)")
	}
	var devs []struct {
		Count   int64 `json:"count"`
		DevMem  int64 `json:"devmem"`
		DevCore int64 `json:"devcore"`
	}
	if err := json.Unmarshal([]byte(ann), &devs); err != nil {
		return GPUUsage{}, fmt.Errorf("%s: %w", nodeRegisterAnnotation, err)
	}
	var u GPUUsage
	for _, d := range devs {
		u.VGPUCapacity += d.Count
		u.MemCapacityMiB += d.DevMem
		u.CoresCapacity += d.DevCore
	}
	return u, nil
}

// DeviceAlloc 是分配给某个容器的一份 vGPU。
type DeviceAlloc struct {
	UUID   string
	MemMiB int64
	Cores  int64
}

// parseAllocated 解析 hami.io/vgpu-devices-allocated。
// 格式：容器之间用 ";" 分隔，同一容器的多张卡用 ":" 分隔，每张卡是 "uuid,类型,显存,算力"。
// 例如 "GPU-0,NVIDIA,10000,30:;" 是一个容器分到一份 10000 MiB、30% 算力。格式不对的项跳过。
func parseAllocated(ann string) []DeviceAlloc {
	var out []DeviceAlloc
	for _, ctr := range strings.Split(ann, ";") {
		for _, dev := range strings.Split(ctr, ":") {
			f := strings.Split(dev, ",")
			if len(f) != 4 || f[0] == "" {
				continue
			}
			mem, err1 := strconv.ParseInt(f[2], 10, 64)
			cores, err2 := strconv.ParseInt(f[3], 10, 64)
			if err1 != nil || err2 != nil {
				continue
			}
			out = append(out, DeviceAlloc{UUID: f[0], MemMiB: mem, Cores: cores})
		}
	}
	return out
}
