package tenant

import "github.com/zhexizx/gputenant/manifests"

var (
	kubeProxyManifest    = manifests.KubeProxy
	requireHamiScheduler = manifests.RequireHamiScheduler
	vapGatePod           = manifests.VAPGatePod
	prometheusRBAC       = manifests.PrometheusRBAC
	prometheusValues     = manifests.PrometheusValues
)

// Steps 是开通一个租户的全部步骤，按执行顺序排列；Delete 按相反顺序执行。
func Steps(e *Env) []Step {
	return []Step{
		// 控制面（原 02-tenant-cp.sh）
		namespaceStep{e},
		controlPlaneStep{e},
		adminKubeconfigStep{e},
		apiProxyStep{e},
		kubeconfigStep{e},
		// 工作节点（原 03-tenant-worker.sh）
		workerStep{e},
		joinStep{e: e},
		kubeProxyStep{e: e},
		calicoStep{e: e},
		imagesStep{e: e},
		nvmlMockStep{e: e},
		hamiStep{e: e},
		// 默认防护（原 05-tenant-guardrails.sh）
		admissionStep{e: e},
		monitoringStep{e: e},
	}
}
