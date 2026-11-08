package tenant

// 工作节点、kube-proxy、Calico、镜像、nvml-mock、HAMi，对应 scripts/03-tenant-worker.sh。
// 这些步骤的资源都在节点容器或租户控制面里，删除时随 worker-node 和 control-plane 一起清理，所以 Delete 是空操作。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type noDelete struct{}

func (noDelete) Delete(context.Context) error { return nil }

// ---- 6. 工作节点容器 ----

type workerStep struct{ e *Env }

func (s workerStep) Name() string { return "worker-node" }

func (s workerStep) Check(ctx context.Context) (bool, error) {
	state, err := containerState(ctx, s.e.Docker, s.e.Cfg.Worker())
	if err != nil || state != "running" {
		return false, err
	}
	// is-system-running 在 starting/degraded 时退出码非 0，这里只看输出。
	out, _ := s.e.Docker.Run(ctx, nil, "exec", s.e.Cfg.Worker(), "systemctl", "is-system-running")
	return out == "running", nil
}

func (s workerStep) Apply(ctx context.Context) error {
	node := s.e.Cfg.Worker()
	state, err := containerState(ctx, s.e.Docker, node)
	if err != nil {
		return err
	}
	switch state {
	case "":
		_, err = s.e.Docker.Run(ctx, nil, "run", "-d", "--name", node, "--hostname", node,
			"--network", "kind", "--privileged",
			"--security-opt", "seccomp=unconfined", "--security-opt", "apparmor=unconfined",
			"--tmpfs", "/tmp", "--tmpfs", "/run", "-v", "/var", "-v", "/lib/modules:/lib/modules:ro",
			NodeImage)
	case "running":
	default:
		_, err = s.e.Docker.Run(ctx, nil, "start", node)
	}
	if err != nil {
		return err
	}
	return waitUntil(ctx, time.Minute, node+" systemd running", s.e.Log, s.Check)
}

func (s workerStep) Delete(ctx context.Context) error {
	return removeContainer(ctx, s.e.Docker, s.e.Cfg.Worker())
}

// ---- 7. 加入租户集群 ----

type joinStep struct {
	e *Env
	noDelete
}

func (s joinStep) Name() string { return "join" }

// Check 同时看节点对象和 kubelet：容器重建后旧的 Node 对象还在，但新容器里还没有 kubelet。
func (s joinStep) Check(ctx context.Context) (bool, error) {
	k, err := s.e.TenantKube()
	if err != nil {
		return false, err
	}
	if _, err := k.Clientset.CoreV1().Nodes().Get(ctx, s.e.Cfg.Worker(), metav1.GetOptions{}); err != nil {
		return false, ignoreNotFound(err)
	}
	out, _ := s.e.Docker.Run(ctx, nil, "exec", s.e.Cfg.Worker(), "systemctl", "is-active", "kubelet")
	return out == "active", nil
}

func (s joinStep) Apply(ctx context.Context) error {
	k, err := s.e.TenantKube()
	if err != nil {
		return err
	}
	node := s.e.Cfg.Worker()
	// 节点有 swap 时必须已经下发 failSwapOn: false，否则 join 要白等 4 分钟才失败（troubleshooting 第 2 条）。
	swap, err := s.e.Docker.Run(ctx, nil, "exec", node, "awk", "/SwapTotal/ {print $2}", "/proc/meminfo")
	if err != nil {
		return err
	}
	cm, err := k.Clientset.CoreV1().ConfigMaps("kube-system").Get(ctx, "kubelet-config", metav1.GetOptions{})
	if err != nil {
		return err
	}
	failSwap := yamlScalar(cm.Data["kubelet"], "failSwapOn")
	s.e.Log("node swap=%skB, kubelet failSwapOn=%s", swap, orDefault(failSwap, "default true"))
	if swap != "0" && failSwap != "false" {
		return fmt.Errorf("node has swap but kubelet-config lacks failSwapOn: false, see troubleshooting #2")
	}

	internal, err := os.ReadFile(s.e.Cfg.InternalKubeconfig())
	if err != nil {
		return err
	}
	tmp := "/root/" + s.e.Cfg.Tenant + ".kubeconfig"
	if _, err := s.e.Docker.Run(ctx, bytes.NewReader(internal), "exec", "-i", node, "sh", "-c", "umask 077; cat > "+tmp); err != nil {
		return err
	}
	defer s.e.Docker.Run(context.WithoutCancel(ctx), nil, "exec", node, "rm", "-f", tmp)
	join, err := s.e.Docker.Run(ctx, nil, "exec", node, "kubeadm", "token", "create", "--print-join-command", "--kubeconfig", tmp)
	if err != nil {
		return err
	}
	// 不打印 join 命令：里面有 bootstrap token。
	if _, err := s.e.Docker.Run(ctx, nil, "exec", node, "bash", "-c", join+" --ignore-preflight-errors=all"); err != nil {
		return fmt.Errorf("kubeadm join failed: %w", err)
	}
	return waitUntil(ctx, time.Minute, node+" registered", s.e.Log, s.Check)
}

// ---- 8. 自管 kube-proxy（--conntrack-max-per-core=0，troubleshooting 第 3 条）----

type kubeProxyStep struct {
	e *Env
	noDelete
}

func (s kubeProxyStep) Name() string { return "kube-proxy" }

// Check 直接看 ClusterIP 规则：rollout status 可能在 kube-proxy 崩溃前就报成功。
func (s kubeProxyStep) Check(ctx context.Context) (bool, error) {
	out, err := s.e.Docker.Run(ctx, nil, "exec", s.e.Cfg.Worker(), "iptables", "-t", "nat", "-S", "KUBE-SERVICES")
	if err != nil {
		return false, nil
	}
	return strings.Contains(out, "default/kubernetes"), nil
}

func renderKubeProxy(tmpl, lbIP string) string {
	return strings.NewReplacer("${LB_IP}", lbIP, "${K8S_VERSION}", K8sVersion, "${POD_CIDR}", PodCIDR).Replace(tmpl)
}

func (s kubeProxyStep) Apply(ctx context.Context) error {
	lbIP, err := s.e.LBIP(ctx)
	if err != nil {
		return err
	}
	k, err := s.e.TenantKube()
	if err != nil {
		return err
	}
	if err := k.ApplyYAML(ctx, []byte(renderKubeProxy(kubeProxyManifest, lbIP))); err != nil {
		return err
	}
	return waitUntil(ctx, 2*time.Minute, "kube-proxy rules written", s.e.Log, s.Check)
}

// ---- 9. Calico ----

type calicoStep struct {
	e *Env
	noDelete
}

func (s calicoStep) Name() string { return "calico" }

func (s calicoStep) Check(ctx context.Context) (bool, error) {
	k, err := s.e.TenantKube()
	if err != nil {
		return false, err
	}
	node, err := k.Clientset.CoreV1().Nodes().Get(ctx, s.e.Cfg.Worker(), metav1.GetOptions{})
	if err != nil {
		return false, ignoreNotFound(err)
	}
	if !nodeReady(node) {
		return false, nil
	}
	ds, err := k.Clientset.AppsV1().DaemonSets("kube-system").Get(ctx, "calico-node", metav1.GetOptions{})
	if err != nil {
		return false, ignoreNotFound(err)
	}
	if !daemonSetRolledOut(ds) {
		return false, nil
	}
	// 看 Pod 而不是 rollout status：节点晚于 Deployment 就绪时，进度时限可能已经过期。
	return anyPodReady(ctx, k, "kube-system", "k8s-app=calico-kube-controllers")
}

func (s calicoStep) Apply(ctx context.Context) error {
	manifest, err := s.manifest(ctx)
	if err != nil {
		return err
	}
	k, err := s.e.TenantKube()
	if err != nil {
		return err
	}
	if err := k.ApplyYAML(ctx, manifest); err != nil {
		return err
	}
	return waitUntil(ctx, 5*time.Minute, "node Ready and Calico running", s.e.Log, s.Check)
}

// manifest 下载（有缓存时用缓存）并写入 POD_CIDR。缓存文件保持原样，只在内存里改。
func (s calicoStep) manifest(ctx context.Context) ([]byte, error) {
	path := filepath.Join(s.e.Cfg.CacheDir, "calico-"+CalicoVersion+".yaml")
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		url := "https://raw.githubusercontent.com/projectcalico/calico/" + CalicoVersion + "/manifests/calico.yaml"
		s.e.Log("download %s", url)
		if data, err = download(ctx, url); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(s.e.Cfg.CacheDir, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
	}
	return patchCalicoCIDR(data, PodCIDR)
}

var calicoCIDR = regexp.MustCompile(`(?m)^([ \t]*)#?[ \t]*- name: CALICO_IPV4POOL_CIDR\n[ \t]*#?[ \t]*value: ".*"$`)

// patchCalicoCIDR 取消 CALICO_IPV4POOL_CIDR 的注释并设成 cidr。注释状态和已有的值都能处理。
func patchCalicoCIDR(manifest []byte, cidr string) ([]byte, error) {
	if n := len(calicoCIDR.FindAllIndex(manifest, -1)); n != 1 {
		return nil, fmt.Errorf("expected exactly one CALICO_IPV4POOL_CIDR entry in Calico manifest, found %d", n)
	}
	repl := fmt.Sprintf("${1}- name: CALICO_IPV4POOL_CIDR\n${1}  value: %q", cidr)
	return calicoCIDR.ReplaceAll(manifest, []byte(repl)), nil
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// ---- 10. 导入本地镜像 ----

type imagesStep struct {
	e *Env
	noDelete
}

func (s imagesStep) Name() string { return "images" }

func (s imagesStep) Check(ctx context.Context) (bool, error) {
	out, err := s.e.Docker.Run(ctx, nil, "exec", s.e.Cfg.Worker(), "ctr", "-n", "k8s.io", "images", "ls", "-q")
	if err != nil {
		return false, nil
	}
	for _, img := range LocalImages {
		if !containsLine(out, "docker.io/library/"+img) {
			return false, nil
		}
	}
	return true, nil
}

func (s imagesStep) Apply(ctx context.Context) error {
	return s.e.Docker.SaveToNode(ctx, s.e.Cfg.Worker(), LocalImages...)
}

// ---- 11. nvml-mock（模拟 GPU）----

type nvmlMockStep struct {
	e *Env
	noDelete
}

func (s nvmlMockStep) Name() string { return "nvml-mock" }

func (s nvmlMockStep) args() []string {
	return []string{"--version", NvmlMockChartVersion,
		"--set", "image.repository=nvml-mock", "--set", "image.tag=local",
		"--set", "gpu.profile=" + s.e.Cfg.GPUProfile, "--set", "gpu.count=" + strconv.Itoa(s.e.Cfg.GPUCount)}
}

func (s nvmlMockStep) Check(ctx context.Context) (bool, error) {
	ok, err := helmDeployed(ctx, s.e.Helm, s.e.Cfg.Kubeconfig(), "default", "nvml-mock")
	if err != nil || !ok {
		return false, err
	}
	return nodeHasLabel(ctx, s.e, "nvidia.com/gpu.present", "true")
}

func (s nvmlMockStep) Apply(ctx context.Context) error {
	// Helm 会静默忽略不存在的键，所以安装前先看渲染出来的镜像。
	rendered, err := s.e.Helm.Run(ctx, append([]string{"template", "nvml-mock", NvmlMockChart}, s.args()...)...)
	if err != nil {
		return err
	}
	if err := checkNvmlRender(rendered); err != nil {
		return err
	}
	if _, err := s.e.Helm.Run(ctx, append([]string{"upgrade", "--install", "nvml-mock", NvmlMockChart,
		"--kubeconfig", s.e.Cfg.Kubeconfig(), "--wait", "--timeout", "180s"}, s.args()...)...); err != nil {
		return err
	}
	// chart 0.6.0 不会自动打这个标签，它的 NOTES 要求手动打。
	return labelNode(ctx, s.e, "nvidia.com/gpu.present", "true")
}

// ---- 12. HAMi ----

type hamiStep struct {
	e *Env
	noDelete
}

func (s hamiStep) Name() string { return "hami" }

func hamiArgs() []string {
	return []string{
		"--set", "devicePlugin.image.repository=hami", "--set", "devicePlugin.image.tag=local",
		"--set", "scheduler.extender.image.repository=hami", "--set", "scheduler.extender.image.tag=local",
		"--set", "devicePlugin.monitor.image.repository=hami", "--set", "devicePlugin.monitor.image.tag=local",
		"--set", "devicePlugin.nvidiaDriverRoot=/var/lib/nvml-mock/driver",
		"--set", "scheduler.kubeScheduler.image.tag=" + K8sVersion,
	}
}

const (
	discoveryEnv   = "DEVICE_DISCOVERY_STRATEGY"
	discoveryValue = "nvml"
)

func (s hamiStep) Check(ctx context.Context) (bool, error) {
	ok, err := helmDeployed(ctx, s.e.Helm, s.e.Cfg.Kubeconfig(), "kube-system", "hami")
	if err != nil || !ok {
		return false, err
	}
	if ok, err := nodeHasLabel(ctx, s.e, "gpu", "on"); err != nil || !ok {
		return false, err
	}
	k, err := s.e.TenantKube()
	if err != nil {
		return false, err
	}
	ds, err := k.Clientset.AppsV1().DaemonSets("kube-system").Get(ctx, "hami-device-plugin", metav1.GetOptions{})
	if err != nil {
		return false, ignoreNotFound(err)
	}
	if containerEnv(ds.Spec.Template.Spec.Containers, "device-plugin", discoveryEnv) != discoveryValue {
		return false, nil
	}
	// 以节点容量为准，不用 rollout status：vgpu-monitor 可能崩溃。
	got, err := gpuCapacity(ctx, k, s.e.Cfg.Worker())
	return got == int64(s.e.Cfg.ExpectedGPU()), err
}

func (s hamiStep) Apply(ctx context.Context) error {
	chart := s.e.Cfg.HAMiChart
	rendered, err := s.e.Helm.Run(ctx, append([]string{"template", "hami", chart, "-n", "kube-system"}, hamiArgs()...)...)
	if err != nil {
		return err
	}
	if err := checkHAMiRender(rendered); err != nil {
		return err
	}
	if _, err := s.e.Helm.Run(ctx, append([]string{"upgrade", "--install", "hami", chart, "-n", "kube-system",
		"--kubeconfig", s.e.Cfg.Kubeconfig()}, hamiArgs()...)...); err != nil {
		return err
	}
	if err := labelNode(ctx, s.e, "gpu", "on"); err != nil {
		return err
	}
	k, err := s.e.TenantKube()
	if err != nil {
		return err
	}
	// 修改 Pod 模板会触发滚动更新，不用再额外 restart。
	patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":"device-plugin","env":[{"name":%q,"value":%q}]}]}}}}`,
		discoveryEnv, discoveryValue)
	if _, err := k.Clientset.AppsV1().DaemonSets("kube-system").Patch(ctx, "hami-device-plugin",
		types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: fieldManager}); err != nil {
		return err
	}

	want := int64(s.e.Cfg.ExpectedGPU())
	if err := waitUntil(ctx, 5*time.Minute, fmt.Sprintf("%s nvidia.com/gpu=%d", s.e.Cfg.Worker(), want), s.e.Log,
		func(ctx context.Context) (bool, error) {
			got, err := gpuCapacity(ctx, k, s.e.Cfg.Worker())
			return got == want, err
		}); err != nil {
		return err
	}
	// device-plugin 只等主容器：vgpu-monitor 可能崩溃。
	if err := waitUntil(ctx, 3*time.Minute, "device-plugin container ready", s.e.Log,
		func(ctx context.Context) (bool, error) { return devicePluginReady(ctx, k) }); err != nil {
		return err
	}
	if err := waitUntil(ctx, 5*time.Minute, "hami-scheduler rolled out", s.e.Log,
		func(ctx context.Context) (bool, error) {
			dep, err := k.Clientset.AppsV1().Deployments("kube-system").Get(ctx, "hami-scheduler", metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return deploymentRolledOut(dep), nil
		}); err != nil {
		return err
	}
	if err := checkLocalHAMiImages(ctx, k); err != nil {
		return err
	}
	// 这个 commit 启动时总会打印 mig 相关的 flag，所以只看策略是不是 none。
	return waitUntil(ctx, time.Minute, "device-plugin migStrategy is none", s.e.Log,
		func(ctx context.Context) (bool, error) { return migStrategyNone(ctx, k) })
}

// ---- Helm 渲染检查（原 scripts/check-helm-render.sh）----

var imageLine = regexp.MustCompile(`(?m)^\s*-?\s*image:\s*(.+)$`)

func renderedImages(rendered string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range imageLine.FindAllStringSubmatch(rendered, -1) {
		img := strings.Trim(strings.TrimSpace(m[1]), `"'`)
		if !seen[img] {
			seen[img] = true
			out = append(out, img)
		}
	}
	return out
}

func checkNvmlRender(rendered string) error {
	for _, img := range renderedImages(rendered) {
		if strings.Contains(img, "nvml-mock:local") {
			return nil
		}
	}
	return fmt.Errorf("nvml-mock render has no nvml-mock:local image (images: %v), check chart keys", renderedImages(rendered))
}

func checkHAMiRender(rendered string) error {
	imgs := renderedImages(rendered)
	var hasLocal, hasScheduler bool
	for _, img := range imgs {
		// 证书 Job 用上游 certgen 是正常的，只拦 projecthami/ 的工作负载镜像。
		if strings.Contains(img, "projecthami/") {
			return fmt.Errorf("HAMi render still uses upstream image %s, check chart keys", img)
		}
		hasLocal = hasLocal || strings.Contains(img, "hami:local")
		hasScheduler = hasScheduler || strings.Contains(img, "kube-scheduler:"+K8sVersion)
	}
	switch {
	case !hasLocal:
		return fmt.Errorf("HAMi render has no hami:local image (images: %v)", imgs)
	case !hasScheduler:
		return fmt.Errorf("HAMi render kube-scheduler is not %s (images: %v)", K8sVersion, imgs)
	case !strings.Contains(rendered, "/var/lib/nvml-mock/driver"):
		return fmt.Errorf("HAMi render ignored nvidiaDriverRoot")
	}
	return nil
}

// checkLocalHAMiImages 确认集群里跑的是 hami:local，避免 chart 键写错后静默用上游镜像。
func checkLocalHAMiImages(ctx context.Context, k *Kube) error {
	dep, err := k.Clientset.AppsV1().Deployments("kube-system").Get(ctx, "hami-scheduler", metav1.GetOptions{})
	if err != nil {
		return err
	}
	ds, err := k.Clientset.AppsV1().DaemonSets("kube-system").Get(ctx, "hami-device-plugin", metav1.GetOptions{})
	if err != nil {
		return err
	}
	for _, c := range append(dep.Spec.Template.Spec.Containers, ds.Spec.Template.Spec.Containers...) {
		if strings.Contains(c.Image, "projecthami") {
			return fmt.Errorf("HAMi container %s still uses upstream image %s", c.Name, c.Image)
		}
	}
	return nil
}

func devicePluginReady(ctx context.Context, k *Kube) (bool, error) {
	pods, err := k.Clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=hami-device-plugin"})
	if err != nil {
		return false, err
	}
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == "device-plugin" && cs.Ready {
				return true, nil
			}
		}
	}
	return false, nil
}

var migNone = regexp.MustCompile(`"migStrategy": *"none"`)

func migStrategyNone(ctx context.Context, k *Kube) (bool, error) {
	pods, err := k.Clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=hami-device-plugin"})
	if err != nil {
		return false, err
	}
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning {
			continue
		}
		logs, err := k.Clientset.CoreV1().Pods("kube-system").GetLogs(p.Name, &corev1.PodLogOptions{Container: "device-plugin"}).DoRaw(ctx)
		if err != nil {
			return false, err
		}
		if migNone.Match(logs) {
			return true, nil
		}
	}
	return false, nil
}

// ---- 节点和工作负载的小工具 ----

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func gpuCapacity(ctx context.Context, k *Kube, node string) (int64, error) {
	n, err := k.Clientset.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	q, ok := n.Status.Capacity["nvidia.com/gpu"]
	if !ok {
		return 0, nil
	}
	return q.Value(), nil
}

func nodeHasLabel(ctx context.Context, e *Env, key, value string) (bool, error) {
	k, err := e.TenantKube()
	if err != nil {
		return false, err
	}
	n, err := k.Clientset.CoreV1().Nodes().Get(ctx, e.Cfg.Worker(), metav1.GetOptions{})
	if err != nil {
		return false, ignoreNotFound(err)
	}
	return n.Labels[key] == value, nil
}

func labelNode(ctx context.Context, e *Env, key, value string) error {
	k, err := e.TenantKube()
	if err != nil {
		return err
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{key: value}}})
	_, err = k.Clientset.CoreV1().Nodes().Patch(ctx, e.Cfg.Worker(), types.MergePatchType, patch, metav1.PatchOptions{FieldManager: fieldManager})
	return err
}

func anyPodReady(ctx context.Context, k *Kube, ns, selector string) (bool, error) {
	pods, err := k.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return false, err
	}
	for i := range pods.Items {
		if podReady(&pods.Items[i]) {
			return true, nil
		}
	}
	return false, nil
}

func containerEnv(cs []corev1.Container, container, name string) string {
	for _, c := range cs {
		if c.Name != container {
			continue
		}
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value
			}
		}
	}
	return ""
}

func deploymentRolledOut(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	return want > 0 && d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas == want && d.Status.AvailableReplicas == want && d.Status.Replicas == want
}

func daemonSetRolledOut(ds *appsv1.DaemonSet) bool {
	st := ds.Status
	return st.DesiredNumberScheduled > 0 && st.ObservedGeneration >= ds.Generation &&
		st.NumberReady == st.DesiredNumberScheduled && st.UpdatedNumberScheduled == st.DesiredNumberScheduled
}

// yamlScalar 从扁平的 YAML 文本里取顶层键的值（kubelet 配置就是这种格式）。
func yamlScalar(doc, key string) string {
	for _, l := range strings.Split(doc, "\n") {
		if v, ok := strings.CutPrefix(l, key+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
