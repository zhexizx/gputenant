package tenant

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// 版本和镜像与 scripts/lib.sh 保持一致，versions_test.go 会比对。
const (
	K8sVersion           = "v1.36.4"
	NodeImage            = "kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed"
	PodCIDR              = "10.244.0.0/16"
	SocatImage           = "alpine/socat:1.8.0.3"
	CalicoVersion        = "v3.31.2"
	NvmlMockChart        = "oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock"
	NvmlMockChartVersion = "0.6.0"
	PrometheusChartRepo  = "https://prometheus-community.github.io/helm-charts"
	PrometheusChartVer   = "29.40.3"

	TenantsNamespace = "tenants"
	FirstProxyPort   = 16443
	// HAMi 默认把每张卡切成 10 份。
	gpuSplit = 10
)

// LocalImages 是 00-build-images.sh 构建、开通时导入工作节点的镜像。
var LocalImages = []string{"nvml-mock:local", "hami:local"}

// StateAlerts 反映持续的故障状态；HamiWebhookFailOpen 是事件型，放行后约 5 分钟才解除。
var (
	StateAlerts     = []string{"KonnectivityAgentDisconnected", "ApiserverToNodePathDown", "HamiWebhookDown"}
	GuardrailAlerts = append(append([]string{}, StateAlerts...), "HamiWebhookFailOpen")
)

// Config 是一个租户的开通参数和本地路径。
type Config struct {
	Tenant     string
	GPUProfile string // a100 或 t4，只有 create 用到
	GPUCount   int
	Home       string // 存放各租户 kubeconfig 的目录
	CacheDir   string // 下载缓存（Calico manifest）
	HAMiChart  string // HAMi chart 目录，按固定 commit 检出
}

var tenantName = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$`)

// ValidateName 检查租户名：它会出现在容器名、TCP 名和 Service 名里。
func ValidateName(name string) error {
	if !tenantName.MatchString(name) {
		return fmt.Errorf("tenant name %q must be a DNS label (lowercase letters, digits, '-'), at most 32 chars", name)
	}
	return nil
}

// ValidateGPU 检查 create 的 GPU 参数。
func (c Config) ValidateGPU() error {
	if c.GPUProfile != "a100" && c.GPUProfile != "t4" {
		return fmt.Errorf("--gpu-profile must be a100 or t4, got %q", c.GPUProfile)
	}
	if c.GPUCount < 1 || c.GPUCount > 8 {
		return fmt.Errorf("--gpu-count must be between 1 and 8, got %d", c.GPUCount)
	}
	return nil
}

// DefaultHome 是 $GPUTENANT_HOME，没设置时为 ~/.gputenant。
func DefaultHome() (string, error) {
	if h := os.Getenv("GPUTENANT_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gputenant"), nil
}

func (c Config) Dir() string                { return filepath.Join(c.Home, c.Tenant) }
func (c Config) Kubeconfig() string         { return filepath.Join(c.Dir(), "kubeconfig") }
func (c Config) InternalKubeconfig() string { return filepath.Join(c.Dir(), "internal.kubeconfig") }
func (c Config) Worker() string             { return c.Tenant + "-worker" }
func (c Config) Proxy() string              { return c.Tenant + "-api-proxy" }
func (c Config) ExpectedGPU() int           { return c.GPUCount * gpuSplit }
