package backing

import (
	"context"
	"testing"
)

type kindRecorder struct{ kinds []string }

func (k *kindRecorder) Start(_ context.Context, spec Spec) (Endpoint, error) {
	k.kinds = append(k.kinds, spec.Kind)
	return Endpoint{}, nil
}
func (k *kindRecorder) Stop(_ context.Context, kind, _ string) error {
	k.kinds = append(k.kinds, kind)
	return nil
}
func (k *kindRecorder) Running(_ context.Context, kind, _ string) (bool, error) {
	k.kinds = append(k.kinds, kind)
	return false, nil
}
func (k *kindRecorder) StopAll(_ context.Context, kind string) error {
	k.kinds = append(k.kinds, kind)
	return nil
}
func (k *kindRecorder) Export(_ context.Context, kind, _ string, _ Data, _ string) error {
	k.kinds = append(k.kinds, kind)
	return nil
}

func TestScopedRunnerTagsEveryKind(t *testing.T) {
	rec := &kindRecorder{}
	r := Scoped(rec, "111122223333.eu-west-1")
	ctx := context.Background()
	_, _ = r.Start(ctx, Spec{Kind: "rds", Name: "db"})
	_ = r.Stop(ctx, "rds", "db")
	_, _ = r.Running(ctx, "rds", "db")
	_ = r.StopAll(ctx, "rds")
	_ = r.Export(ctx, "rds", "db", Data{}, "")
	for _, kind := range rec.kinds {
		if kind != "rds.111122223333.eu-west-1" {
			t.Fatalf("kind = %q", kind)
		}
	}
	if len(rec.kinds) != 5 {
		t.Fatalf("calls = %d", len(rec.kinds))
	}
}

func TestScopedRunnerWithoutTagIsInner(t *testing.T) {
	rec := &kindRecorder{}
	if Scoped(rec, "") != Runner(rec) {
		t.Fatal("an empty tag should return the inner runner")
	}
}
