package tenant

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

const fieldManager = "gputenant"

// Kube 是一个集群的客户端：typed client 读写内置资源，dynamic client 处理 CRD 和任意 YAML。
type Kube struct {
	Clientset kubernetes.Interface
	Dynamic   dynamic.Interface
	mapper    *restmapper.DeferredDiscoveryRESTMapper
}

// NewKube 从 kubeconfig 文件创建客户端。
func NewKube(kubeconfig string) (*Kube, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return newKubeFromConfig(cfg)
}

func newKubeFromConfig(cfg *rest.Config) (*Kube, error) {
	// Check 要快：status 需要几秒内返回，所以单次请求不能挂很久。
	cfg.Timeout = 30 * time.Second
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(cs.Discovery()))
	return &Kube{Clientset: cs, Dynamic: dyn, mapper: mapper}, nil
}

// resource 找到对象对应的 REST 资源。刚装的 CRD 不在缓存里，找不到时刷新一次。
func (k *Kube) resource(obj *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	gvk := obj.GroupVersionKind()
	mapping, err := k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		k.mapper.Reset()
		mapping, err = k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	if err != nil {
		return nil, err
	}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = metav1.NamespaceDefault
		}
		return k.Dynamic.Resource(mapping.Resource).Namespace(ns), nil
	}
	return k.Dynamic.Resource(mapping.Resource), nil
}

// ApplyObject 用 server-side apply 创建或更新对象，重复执行结果不变。
func (k *Kube) ApplyObject(ctx context.Context, obj *unstructured.Unstructured) error {
	ri, err := k.resource(obj)
	if err != nil {
		return fmt.Errorf("%s/%s: %w", obj.GetKind(), obj.GetName(), err)
	}
	data, err := obj.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	_, err = ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, data,
		metav1.PatchOptions{FieldManager: fieldManager, Force: &force})
	if err != nil {
		return fmt.Errorf("apply %s/%s: %w", obj.GetKind(), obj.GetName(), err)
	}
	return nil
}

// ApplyYAML 逐个 apply 多文档 YAML 里的对象。
func (k *Kube) ApplyYAML(ctx context.Context, data []byte) error {
	objs, err := DecodeYAML(data)
	if err != nil {
		return err
	}
	for _, obj := range objs {
		if err := k.ApplyObject(ctx, obj); err != nil {
			return err
		}
	}
	return nil
}

// DryRunCreate 以 dry-run 方式在服务端创建对象，用来确认准入策略会不会拒绝它。
func (k *Kube) DryRunCreate(ctx context.Context, obj *unstructured.Unstructured) error {
	ri, err := k.resource(obj)
	if err != nil {
		return err
	}
	_, err = ri.Create(ctx, obj, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	return err
}

// Get 读取任意资源，不存在时返回 nil, nil。
func (k *Kube) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	var ri dynamic.ResourceInterface = k.Dynamic.Resource(gvr)
	if ns != "" {
		ri = k.Dynamic.Resource(gvr).Namespace(ns)
	}
	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return obj, err
}

// DecodeYAML 把多文档 YAML 解析成对象，跳过空文档（例如只有注释的段落）。
func DecodeYAML(data []byte) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objs []*unstructured.Unstructured
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return objs, nil
			}
			return nil, err
		}
		if len(m) == 0 {
			continue
		}
		objs = append(objs, &unstructured.Unstructured{Object: m})
	}
}

// waitUntil 每 5 秒检查一次 cond，直到返回 true 或超时。
// cond 返回的错误不会立即中止等待（集群刚起来时请求失败很正常），超时时一起报出来。
func waitUntil(ctx context.Context, timeout time.Duration, desc string, logf Logf, cond func(context.Context) (bool, error)) error {
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		ok, err := cond(ctx)
		if err != nil {
			lastErr = err
			return false, nil
		}
		return ok, nil
	})
	if err != nil {
		// 外层 ctx 被取消（Ctrl-C）时不要报成超时。
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted while waiting for %s: %w", desc, ctx.Err())
		}
		if lastErr != nil {
			return fmt.Errorf("timeout (%s) waiting for %s: last error: %w", timeout, desc, lastErr)
		}
		return fmt.Errorf("timeout (%s) waiting for %s", timeout, desc)
	}
	logf("ok: %s", desc)
	return nil
}
