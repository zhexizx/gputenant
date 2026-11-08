package tenant

// 租户控制面，对应 scripts/02-tenant-cp.sh。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
)

// ---- 1. tenants namespace（所有租户共用，删除时保留）----

type namespaceStep struct{ e *Env }

func (s namespaceStep) Name() string { return "tenants-namespace" }

func (s namespaceStep) Check(ctx context.Context) (bool, error) {
	_, err := s.e.Mgmt.Clientset.CoreV1().Namespaces().Get(ctx, TenantsNamespace, metav1.GetOptions{})
	if err != nil {
		return false, ignoreNotFound(err)
	}
	return true, nil
}

func (s namespaceStep) Apply(ctx context.Context) error {
	ns := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": TenantsNamespace},
	}}
	return s.e.Mgmt.ApplyObject(ctx, ns)
}

func (s namespaceStep) Delete(context.Context) error { return nil }

// ---- 2. TenantControlPlane ----

type controlPlaneStep struct{ e *Env }

func (s controlPlaneStep) Name() string { return "control-plane" }

// tenantControlPlane 生成 TCP 对象。不启用 kubeProxy addon，由 kube-proxy 步骤自管（troubleshooting 第 3 条）。
func tenantControlPlane(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kamaji.clastix.io/v1alpha1",
		"kind":       "TenantControlPlane",
		"metadata": map[string]any{
			"name": name, "namespace": TenantsNamespace,
			"labels": map[string]any{"tenant.clastix.io": name},
		},
		"spec": map[string]any{
			"controlPlane": map[string]any{
				"deployment": map[string]any{"replicas": int64(1)},
				"service":    map[string]any{"serviceType": "LoadBalancer"},
			},
			"kubernetes": map[string]any{
				"version": K8sVersion,
				"kubelet": map[string]any{
					"configurationJSONPatches": []any{
						map[string]any{"op": "add", "path": "/cgroupDriver", "value": "systemd"},
						// 和 kind 自己的节点配置一致：Docker Desktop VM 开了 swap，默认 true 时 kubelet 起不来。
						map[string]any{"op": "add", "path": "/failSwapOn", "value": false},
					},
				},
			},
			"networkProfile": map[string]any{"port": int64(6443), "podCidr": PodCIDR},
			"addons": map[string]any{
				"coreDNS": map[string]any{},
				"konnectivity": map[string]any{
					"server": map[string]any{"port": int64(8132)},
					"agent":  map[string]any{"mode": "DaemonSet"},
				},
			},
		},
	}}
}

func (s controlPlaneStep) Check(ctx context.Context) (bool, error) {
	tcp, err := s.e.Mgmt.Get(ctx, tcpGVR, TenantsNamespace, s.e.Cfg.Tenant)
	if err != nil || tcp == nil {
		return false, err
	}
	// 启用 addon 后 Kamaji 会管理 kube-proxy，并还原对它的修改。
	if _, found, _ := unstructured.NestedMap(tcp.Object, "spec", "addons", "kubeProxy"); found {
		return false, fmt.Errorf("TenantControlPlane %s has the kubeProxy addon enabled, see troubleshooting #3", s.e.Cfg.Tenant)
	}
	status, _, _ := unstructured.NestedString(tcp.Object, "status", "kubernetesResources", "version", "status")
	if status != "Ready" {
		return false, nil
	}
	dep, err := s.e.Mgmt.Clientset.AppsV1().Deployments(TenantsNamespace).Get(ctx, s.e.Cfg.Tenant, metav1.GetOptions{})
	if err != nil {
		return false, ignoreNotFound(err)
	}
	return deploymentRolledOut(dep), nil
}

func (s controlPlaneStep) Apply(ctx context.Context) error {
	if err := s.e.Mgmt.ApplyObject(ctx, tenantControlPlane(s.e.Cfg.Tenant)); err != nil {
		return err
	}
	return waitUntil(ctx, 8*time.Minute, s.e.Cfg.Tenant+" control plane Ready", s.e.Log, s.Check)
}

func (s controlPlaneStep) Delete(ctx context.Context) error {
	ri := s.e.Mgmt.Dynamic.Resource(tcpGVR).Namespace(TenantsNamespace)
	if err := ignoreNotFound(ri.Delete(ctx, s.e.Cfg.Tenant, metav1.DeleteOptions{})); err != nil {
		return err
	}
	// 等 Kamaji 的 finalizer 清理完数据存储，否则马上重建同名租户会读到旧数据。
	return waitUntil(ctx, 5*time.Minute, "TenantControlPlane "+s.e.Cfg.Tenant+" deleted", s.e.Log,
		func(ctx context.Context) (bool, error) {
			tcp, err := s.e.Mgmt.Get(ctx, tcpGVR, TenantsNamespace, s.e.Cfg.Tenant)
			return tcp == nil, err
		})
}

// ---- 3. admin kubeconfig（证书原样、地址是 LB IP，给 kind 网络内的工作节点用）----

type adminKubeconfigStep struct{ e *Env }

func (s adminKubeconfigStep) Name() string { return "admin-kubeconfig" }

func (s adminKubeconfigStep) secret(ctx context.Context) ([]byte, error) {
	sec, err := s.e.Mgmt.Clientset.CoreV1().Secrets(TenantsNamespace).Get(ctx, s.e.Cfg.Tenant+"-admin-kubeconfig", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	data := sec.Data["admin.conf"]
	if len(data) == 0 {
		return nil, fmt.Errorf("secret %s-admin-kubeconfig has no admin.conf", s.e.Cfg.Tenant)
	}
	return data, nil
}

func (s adminKubeconfigStep) Check(ctx context.Context) (bool, error) {
	local, err := os.ReadFile(s.e.Cfg.InternalKubeconfig())
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	// 证书轮换后 secret 会变，文件要跟着更新。
	remote, err := s.secret(ctx)
	if err != nil {
		return false, ignoreNotFound(err)
	}
	return bytes.Equal(local, remote), nil
}

func (s adminKubeconfigStep) Apply(ctx context.Context) error {
	data, err := s.secret(ctx)
	if err != nil {
		return err
	}
	return writePrivate(s.e.Cfg.InternalKubeconfig(), data)
}

// Delete 是最后一个删本地文件的步骤（逆序执行），所以由它删掉租户目录；目录里还有别的文件时保留。
func (s adminKubeconfigStep) Delete(context.Context) error {
	if err := removeFile(s.e.Cfg.InternalKubeconfig()); err != nil {
		return err
	}
	if err := os.Remove(s.e.Cfg.Dir()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.e.Log("keep %s: %v", s.e.Cfg.Dir(), err)
	}
	return nil
}

// ---- 4. socat 代理：Docker Desktop 下 WSL 没有到 kind 网桥的路由，本机经 127.0.0.1 访问 ----

type apiProxyStep struct{ e *Env }

func (s apiProxyStep) Name() string { return "api-proxy" }

func proxyTarget(lbIP string) string { return "TCP:" + lbIP + ":6443" }

func (s apiProxyStep) Check(ctx context.Context) (bool, error) {
	state, err := containerState(ctx, s.e.Docker, s.e.Cfg.Proxy())
	if err != nil || state != "running" {
		return false, err
	}
	lbIP, err := s.e.LBIP(ctx)
	if err != nil {
		return false, nil
	}
	target, err := s.e.Docker.Run(ctx, nil, "inspect", "-f", "{{index .Config.Cmd 1}}", s.e.Cfg.Proxy())
	if err != nil {
		return false, err
	}
	return target == proxyTarget(lbIP), nil
}

func (s apiProxyStep) Apply(ctx context.Context) error {
	lbIP, err := s.e.LBIP(ctx)
	if err != nil {
		return err
	}
	used, err := proxyPorts(ctx, s.e.Docker)
	if err != nil {
		return err
	}
	// 代理已存在但指向旧 IP 时，沿用它的端口，避免租户换端口。
	port, ok := used[s.e.Cfg.Proxy()]
	if !ok {
		taken := map[int]bool{}
		for _, p := range used {
			taken[p] = true
		}
		port = pickPort(taken, FirstProxyPort, portFree)
		if port == 0 {
			return fmt.Errorf("no free proxy port from %d", FirstProxyPort)
		}
	}
	if err := removeContainer(ctx, s.e.Docker, s.e.Cfg.Proxy()); err != nil {
		return err
	}
	s.e.Log("proxy %s: 127.0.0.1:%d -> %s:6443", s.e.Cfg.Proxy(), port, lbIP)
	_, err = s.e.Docker.Run(ctx, nil, "run", "-d", "--name", s.e.Cfg.Proxy(), "--restart", "unless-stopped",
		"--network", "kind", "-p", fmt.Sprintf("127.0.0.1:%d:6443", port),
		SocatImage, "TCP-LISTEN:6443,fork,reuseaddr", proxyTarget(lbIP))
	return err
}

func (s apiProxyStep) Delete(ctx context.Context) error {
	return removeContainer(ctx, s.e.Docker, s.e.Cfg.Proxy())
}

// proxyPorts 返回所有 *-api-proxy 容器（包括已停止的）绑定的本机端口。
func proxyPorts(ctx context.Context, d Docker) (map[string]int, error) {
	names, err := d.Run(ctx, nil, "ps", "-a", "--filter", "name=-api-proxy", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	ports := map[string]int{}
	for _, name := range strings.Fields(names) {
		if !strings.HasSuffix(name, "-api-proxy") {
			continue
		}
		out, err := d.Run(ctx, nil, "inspect", "-f", "{{json .HostConfig.PortBindings}}", name)
		if err != nil {
			return nil, err
		}
		if p := parseHostPort(out); p > 0 {
			ports[name] = p
		}
	}
	return ports, nil
}

// parseHostPort 从 HostConfig.PortBindings 的 JSON 里取 6443/tcp 的本机端口。
func parseHostPort(bindingsJSON string) int {
	var b map[string][]struct{ HostPort string }
	if err := json.Unmarshal([]byte(bindingsJSON), &b); err != nil {
		return 0
	}
	for _, hb := range b["6443/tcp"] {
		if p, err := strconv.Atoi(hb.HostPort); err == nil {
			return p
		}
	}
	return 0
}

// pickPort 从 start 开始找第一个没被代理容器占用、并且本机可监听的端口。
func pickPort(taken map[int]bool, start int, free func(int) bool) int {
	for p := start; p < start+100; p++ {
		if !taken[p] && free(p) {
			return p
		}
	}
	return 0
}

func portFree(p int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// ---- 5. 本机 kubeconfig：经代理走 127.0.0.1，证书校验仍按 LB IP ----

type kubeconfigStep struct{ e *Env }

func (s kubeconfigStep) Name() string { return "kubeconfig" }

func (s kubeconfigStep) want(ctx context.Context) ([]byte, error) {
	lbIP, err := s.e.LBIP(ctx)
	if err != nil {
		return nil, err
	}
	ports, err := proxyPorts(ctx, s.e.Docker)
	if err != nil {
		return nil, err
	}
	port, ok := ports[s.e.Cfg.Proxy()]
	if !ok {
		return nil, fmt.Errorf("proxy container %s not found", s.e.Cfg.Proxy())
	}
	internal, err := os.ReadFile(s.e.Cfg.InternalKubeconfig())
	if err != nil {
		return nil, err
	}
	return rewriteKubeconfig(internal, port, lbIP)
}

func (s kubeconfigStep) Check(ctx context.Context) (bool, error) {
	local, err := os.ReadFile(s.e.Cfg.Kubeconfig())
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	want, err := s.want(ctx)
	if err != nil {
		return false, nil
	}
	if !bytes.Equal(local, want) {
		return false, nil
	}
	return apiserverReady(ctx, s.e)
}

func apiserverReady(ctx context.Context, e *Env) (bool, error) {
	k, err := e.TenantKube()
	if err != nil {
		return false, err
	}
	body, err := k.Clientset.Discovery().RESTClient().Get().AbsPath("/readyz").DoRaw(ctx)
	if err != nil {
		return false, nil
	}
	return string(body) == "ok", nil
}

func (s kubeconfigStep) Apply(ctx context.Context) error {
	want, err := s.want(ctx)
	if err != nil {
		return err
	}
	if err := writePrivate(s.e.Cfg.Kubeconfig(), want); err != nil {
		return err
	}
	return waitUntil(ctx, 2*time.Minute, s.e.Cfg.Tenant+" apiserver reachable via proxy", s.e.Log,
		func(ctx context.Context) (bool, error) { return apiserverReady(ctx, s.e) })
}

func (s kubeconfigStep) Delete(context.Context) error {
	return removeFile(s.e.Cfg.Kubeconfig())
}

// rewriteKubeconfig 把所有 cluster 的地址改成 127.0.0.1:<port>，TLS 校验的名字设成 LB IP。
func rewriteKubeconfig(internal []byte, port int, lbIP string) ([]byte, error) {
	cfg, err := clientcmd.Load(internal)
	if err != nil {
		return nil, err
	}
	if len(cfg.Clusters) == 0 {
		return nil, fmt.Errorf("kubeconfig has no clusters")
	}
	for _, c := range cfg.Clusters {
		c.Server = fmt.Sprintf("https://127.0.0.1:%d", port)
		c.TLSServerName = lbIP
	}
	return clientcmd.Write(*cfg)
}

// ---- 文件工具 ----

func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ---- 就绪判断 ----

func podReady(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
