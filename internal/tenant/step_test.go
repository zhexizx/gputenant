package tenant

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// fakeStep 用一个布尔值模拟集群里资源是否存在。
type fakeStep struct {
	name     string
	exists   bool
	noEffect bool // Apply 不报错但不生效
	applyErr error
	log      *[]string
}

func (f *fakeStep) Name() string { return f.name }

func (f *fakeStep) Check(context.Context) (bool, error) { return f.exists, nil }

func (f *fakeStep) Apply(context.Context) error {
	*f.log = append(*f.log, "apply "+f.name)
	if f.applyErr != nil {
		return f.applyErr
	}
	if !f.noEffect {
		f.exists = true
	}
	return nil
}

func (f *fakeStep) Delete(context.Context) error {
	*f.log = append(*f.log, "delete "+f.name)
	f.exists = false
	return nil
}

func steps(log *[]string, fs ...*fakeStep) []Step {
	out := make([]Step, len(fs))
	for i, f := range fs {
		f.log = log
		out[i] = f
	}
	return out
}

func noop(string, Event) {}

func TestCreateSkipsDoneSteps(t *testing.T) {
	var log []string
	s := steps(&log, &fakeStep{name: "a", exists: true}, &fakeStep{name: "b"})
	if err := Create(context.Background(), s, noop); err != nil {
		t.Fatal(err)
	}
	if want := []string{"apply b"}; !slices.Equal(log, want) {
		t.Fatalf("log = %v, want %v", log, want)
	}
}

func TestCreateRerunAfterFailure(t *testing.T) {
	var log []string
	b := &fakeStep{name: "b", applyErr: errors.New("boom")}
	s := steps(&log, &fakeStep{name: "a"}, b, &fakeStep{name: "c"})
	if err := Create(context.Background(), s, noop); err == nil {
		t.Fatal("want error from step b")
	}
	b.applyErr = nil
	log = nil
	if err := Create(context.Background(), s, noop); err != nil {
		t.Fatal(err)
	}
	if want := []string{"apply b", "apply c"}; !slices.Equal(log, want) {
		t.Fatalf("rerun log = %v, want %v", log, want)
	}
}

func TestCreateDetectsApplyWithoutEffect(t *testing.T) {
	var log []string
	s := steps(&log, &fakeStep{name: "a", noEffect: true})
	if err := Create(context.Background(), s, noop); err == nil {
		t.Fatal("want error when check stays false after apply")
	}
}

func TestDeleteReverseOrder(t *testing.T) {
	var log []string
	s := steps(&log, &fakeStep{name: "a"}, &fakeStep{name: "b"}, &fakeStep{name: "c"})
	if err := Delete(context.Background(), s, noop); err != nil {
		t.Fatal(err)
	}
	if want := []string{"delete c", "delete b", "delete a"}; !slices.Equal(log, want) {
		t.Fatalf("log = %v, want %v", log, want)
	}
}
