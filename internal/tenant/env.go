package tenant

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Logf 打印进度。
type Logf func(format string, args ...any)

// Env 是各个 Step 共用的依赖。
type Env struct {
	Cfg    Config
	Docker Docker
	Helm   Helm
	Mgmt   *Kube // 管理集群
	Log    Logf
}

var (
	tcpGVR = schema.GroupVersionResource{Group: "kamaji.clastix.io", Version: "v1alpha1", Resource: "tenantcontrolplanes"}
	crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
)

// TenantKube 用本地 kubeconfig（经 socat 代理）连接租户集群。每次重新读取，因为开通过程中文件会被改写。
func (e *Env) TenantKube() (*Kube, error) {
	if _, err := os.Stat(e.Cfg.Kubeconfig()); err != nil {
		return nil, fmt.Errorf("tenant kubeconfig: %w", err)
	}
	return NewKube(e.Cfg.Kubeconfig())
}

// LBIP 读取租户 apiserver Service 的 LoadBalancer IP。
func (e *Env) LBIP(ctx context.Context) (string, error) {
	svc, err := e.Mgmt.Clientset.CoreV1().Services(TenantsNamespace).Get(ctx, e.Cfg.Tenant, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if len(svc.Status.LoadBalancer.Ingress) == 0 || svc.Status.LoadBalancer.Ingress[0].IP == "" {
		return "", fmt.Errorf("service %s/%s has no LoadBalancer IP yet", TenantsNamespace, e.Cfg.Tenant)
	}
	return svc.Status.LoadBalancer.Ingress[0].IP, nil
}

// PreflightCreate 只确认 bash 负责的部分已就绪：管理集群（01-management.sh）和本地镜像（00-build-images.sh）。
func (e *Env) PreflightCreate(ctx context.Context) error {
	if err := e.PreflightDelete(ctx); err != nil {
		return err
	}
	dep, err := e.Mgmt.Clientset.AppsV1().Deployments("kamaji-system").Get(ctx, "kamaji", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("kamaji controller not found, run scripts/01-management.sh first: %w", err)
	}
	if dep.Status.AvailableReplicas < 1 {
		return fmt.Errorf("kamaji controller in kamaji-system is not available")
	}
	if _, err := e.Docker.Run(ctx, nil, "network", "inspect", "kind", "--format", "{{.Name}}"); err != nil {
		return fmt.Errorf("docker network kind not found: %w", err)
	}
	for _, img := range LocalImages {
		if _, err := e.Docker.Run(ctx, nil, "image", "inspect", img, "--format", "{{.Id}}"); err != nil {
			return fmt.Errorf("image %s not found, run scripts/00-build-images.sh first", img)
		}
	}
	if _, err := os.Stat(e.Cfg.HAMiChart + "/Chart.yaml"); err != nil {
		return fmt.Errorf("HAMi chart not found at %s, run scripts/00-build-images.sh or pass --hami-chart", e.Cfg.HAMiChart)
	}
	if _, err := e.Helm.Run(ctx, "version", "--short"); err != nil {
		return fmt.Errorf("helm not usable: %w", err)
	}
	return nil
}

// PreflightDelete 确认管理集群可访问、Kamaji CRD 已安装。
func (e *Env) PreflightDelete(ctx context.Context) error {
	crd, err := e.Mgmt.Get(ctx, crdGVR, "", "tenantcontrolplanes.kamaji.clastix.io")
	if err != nil {
		return fmt.Errorf("management cluster not reachable (check --kubeconfig): %w", err)
	}
	if crd == nil {
		return fmt.Errorf("Kamaji CRDs not installed in management cluster, run scripts/01-management.sh first")
	}
	if _, err := exec.LookPath(dockerBin(e.Docker)); err != nil {
		return fmt.Errorf("docker not found: %w", err)
	}
	return nil
}

func dockerBin(d Docker) string {
	if c, ok := d.(CLIDocker); ok {
		return c.Bin
	}
	return "docker"
}

// ignoreNotFound 把 NotFound 当成成功。
func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func containsLine(out, want string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}
