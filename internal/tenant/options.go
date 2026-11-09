package tenant

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Options 是所有租户共用的设置，CLI 从参数读，API 启动时读一次。
type Options struct {
	Kubeconfig string // 管理集群
	HAMiChart  string
	DockerBin  string
	HelmBin    string
	Home       string // 空时用 DefaultHome()
	CacheDir   string // 空时用用户缓存目录下的 gputenant
}

// Complete 填好默认值并检查必填项。
func (o Options) Complete() (Options, error) {
	if o.Kubeconfig == "" {
		return o, errors.New("management cluster kubeconfig not set: pass --kubeconfig or set KUBECONFIG")
	}
	if o.Home == "" {
		h, err := DefaultHome()
		if err != nil {
			return o, err
		}
		o.Home = h
	}
	if o.CacheDir == "" {
		c, err := os.UserCacheDir()
		if err != nil {
			return o, err
		}
		o.CacheDir = filepath.Join(c, "gputenant")
	}
	if o.DockerBin == "" {
		o.DockerBin = "docker"
	}
	if o.HelmBin == "" {
		o.HelmBin = "helm"
	}
	return o, nil
}

// NewEnv 为一个租户创建 Env。o 要先 Complete。cfg 里只需要 Tenant 和（create 时的）GPU 参数。
func NewEnv(o Options, cfg Config, log Logf) (*Env, error) {
	if err := ValidateName(cfg.Tenant); err != nil {
		return nil, err
	}
	cfg.Home, cfg.CacheDir, cfg.HAMiChart = o.Home, o.CacheDir, o.HAMiChart
	mgmt, err := NewKube(o.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("management kubeconfig: %w", err)
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	return &Env{
		Cfg:    cfg,
		Docker: CLIDocker{Bin: o.DockerBin},
		Helm:   CLIHelm{Bin: o.HelmBin},
		Mgmt:   mgmt,
		Log:    log,
	}, nil
}

// ListTenants 列出管理集群里的租户（tenants 命名空间下的 TCP）。
func ListTenants(ctx context.Context, mgmt *Kube) ([]string, error) {
	list, err := mgmt.Dynamic.Resource(tcpGVR).Namespace(TenantsNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		names = append(names, it.GetName())
	}
	sort.Strings(names)
	return names, nil
}
