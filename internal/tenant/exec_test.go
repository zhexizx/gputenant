package tenant

import (
	"context"
	"io"
	"strings"
	"testing"
)

// fakeDocker 记录调用，ps 按 states 返回容器状态。
type fakeDocker struct {
	states map[string]string
	calls  []string
}

func (f *fakeDocker) Run(_ context.Context, _ io.Reader, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if args[0] == "ps" {
		for _, a := range args {
			if name, ok := strings.CutPrefix(a, "name=^/"); ok {
				return f.states[strings.TrimSuffix(name, "$")], nil
			}
		}
	}
	return "", nil
}

func (f *fakeDocker) SaveToNode(context.Context, string, ...string) error { return nil }

func (f *fakeDocker) removed(name string) bool {
	for _, c := range f.calls {
		if c == "rm -fv "+name {
			return true
		}
	}
	return false
}

func TestDeleteContainersToleratesMissing(t *testing.T) {
	d := &fakeDocker{states: map[string]string{}}
	e := &Env{Cfg: Config{Tenant: "t1"}, Docker: d, Log: func(string, ...any) {}}
	for _, s := range []Step{workerStep{e}, apiProxyStep{e}} {
		if err := s.Delete(context.Background()); err != nil {
			t.Fatalf("%s: %v", s.Name(), err)
		}
	}
	if d.removed("t1-worker") || d.removed("t1-api-proxy") {
		t.Fatalf("rm called for missing containers: %v", d.calls)
	}
}

func TestDeleteContainersRemovesStopped(t *testing.T) {
	d := &fakeDocker{states: map[string]string{"t1-worker": "exited", "t1-api-proxy": "running"}}
	e := &Env{Cfg: Config{Tenant: "t1"}, Docker: d, Log: func(string, ...any) {}}
	for _, s := range []Step{workerStep{e}, apiProxyStep{e}} {
		if err := s.Delete(context.Background()); err != nil {
			t.Fatalf("%s: %v", s.Name(), err)
		}
	}
	if !d.removed("t1-worker") || !d.removed("t1-api-proxy") {
		t.Fatalf("containers not removed: %v", d.calls)
	}
}
