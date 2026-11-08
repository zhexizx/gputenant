// Package manifests 把 CLI 用到的 manifest 编进二进制，bash 脚本读的是同一份文件。
package manifests

import _ "embed"

// KubeProxy 里的 ${LB_IP}、${K8S_VERSION}、${POD_CIDR} 由调用方替换。
//
//go:embed kube-proxy.yaml
var KubeProxy string

//go:embed guardrails/require-hami-scheduler.yaml
var RequireHamiScheduler []byte

//go:embed guardrails/vap-gate-pod.yaml
var VAPGatePod []byte

//go:embed guardrails/prometheus-rbac.yaml
var PrometheusRBAC []byte

//go:embed guardrails/prometheus-values.yaml
var PrometheusValues []byte
