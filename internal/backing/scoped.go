package backing

import "context"

// Scoped returns a runner whose engine kinds carry tag, so two accounts or
// regions can each run an engine with the same name, and StopAll for one
// never touches the other's. An empty tag returns r.
func Scoped(r Runner, tag string) Runner {
	if tag == "" {
		return r
	}
	return scopedRunner{inner: r, tag: tag}
}

type scopedRunner struct {
	inner Runner
	tag   string
}

func (s scopedRunner) kind(kind string) string { return kind + "." + s.tag }

func (s scopedRunner) Start(ctx context.Context, spec Spec) (Endpoint, error) {
	spec.Kind = s.kind(spec.Kind)
	return s.inner.Start(ctx, spec)
}

func (s scopedRunner) Stop(ctx context.Context, kind, name string) error {
	return s.inner.Stop(ctx, s.kind(kind), name)
}

func (s scopedRunner) Running(ctx context.Context, kind, name string) (bool, error) {
	return s.inner.Running(ctx, s.kind(kind), name)
}

func (s scopedRunner) StopAll(ctx context.Context, kind string) error {
	return s.inner.StopAll(ctx, s.kind(kind))
}

func (s scopedRunner) Export(ctx context.Context, kind, name string, data Data, dst string) error {
	return s.inner.Export(ctx, s.kind(kind), name, data, dst)
}
