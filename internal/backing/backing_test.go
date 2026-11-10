package backing

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

type exportRunner struct {
	Unavailable
	errs     map[string]error
	exported map[string]string
}

func (r *exportRunner) Export(_ context.Context, _, name string, _ Data, dst string) error {
	if err := r.errs[name]; err != nil {
		return err
	}
	r.exported[name] = dst
	return nil
}

func TestExportAllSkipsEnginesWithNothingToCapture(t *testing.T) {
	dir := t.TempDir()
	runner := &exportRunner{
		errs: map[string]error{
			"stopped":  ErrNotRunning,
			"nodocker": ErrUnavailable,
		},
		exported: map[string]string{},
	}

	got, err := ExportAll(context.Background(), runner, "rds", dir, []Exportable{
		{Name: "orders"}, {Name: "stopped"}, {Name: "nodocker"}, {Name: "users"},
	})
	if err != nil {
		t.Fatalf("export all: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"orders", "users"}) {
		t.Fatalf("captured = %v", got)
	}
	if runner.exported["orders"] != filepath.Join(dir, "orders.tar.gz") {
		t.Fatalf("archive should land in dir, got %q", runner.exported["orders"])
	}
}

func TestExportAllFailsOnRealErrors(t *testing.T) {
	runner := &exportRunner{errs: map[string]error{"orders": errors.New("disk full")}, exported: map[string]string{}}

	_, err := ExportAll(context.Background(), runner, "rds", t.TempDir(), []Exportable{{Name: "orders"}})
	if err == nil || !errors.Is(err, runner.errs["orders"]) {
		t.Fatalf("a failed export should fail the capture, got %v", err)
	}
}
