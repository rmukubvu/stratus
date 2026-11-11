package app

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stratus/internal/backing"
	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/control"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/services/bedrock"
	"github.com/stratus/internal/services/sts"
	"github.com/stratus/internal/store/bbolt"
	"github.com/stratus/internal/store/fsblob"
)

func newTestStacks(t *testing.T) *Stacks {
	t.Helper()
	dir := t.TempDir()
	meta, err := bbolt.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := chaos.NewEngine(control.DefaultCatalog().Names(), nil)
	stacks := newStacks(platform{
		meta:    meta,
		blobs:   fsblob.New(filepath.Join(dir, "blobs")),
		engines: backing.Unavailable{},
		faults:  chaos.NewInjector(engine, nil, logger),
		chaos:   engine,
		bedrock: bedrock.NewService(bedrock.Options{Metadata: meta}),
		sts:     sts.NewService(),
		getenv:  func(string) string { return "" },
		logger:  logger,
	})
	t.Cleanup(func() { _ = stacks.Close() })
	return stacks
}

func TestStacksAreBuiltOncePerScope(t *testing.T) {
	stacks := newTestStacks(t)
	a := scope.Scope{Account: "111122223333", Region: "eu-west-1"}
	if stacks.For(a) != stacks.For(a) {
		t.Fatal("the same scope should get the same services")
	}
	if stacks.For(a).SQS == stacks.For(scope.Default()).SQS {
		t.Fatal("different scopes should get different regional services")
	}
}

func TestGlobalServicesAreSharedAcrossRegions(t *testing.T) {
	stacks := newTestStacks(t)
	west := stacks.For(scope.Scope{Account: "111122223333", Region: "eu-west-1"})
	east := stacks.For(scope.Scope{Account: "111122223333", Region: "us-east-1"})
	other := stacks.For(scope.Scope{Account: "444455556666", Region: "eu-west-1"})
	if west.IAM != east.IAM || west.S3 != east.S3 || west.Route53 != east.Route53 {
		t.Fatal("IAM, S3 and Route 53 are global to an account")
	}
	if west.IAM == other.IAM || west.S3 == other.S3 {
		t.Fatal("global services must not be shared across accounts")
	}
}

func TestScopesInFindsEveryNonDefaultScope(t *testing.T) {
	got := scopesIn([]string{
		"sqs-queues",
		"@111122223333/eu-west-1/sqs-queues",
		"@111122223333/eu-west-1/sns-topics",
		"@444455556666/us-east-1/iam-roles",
	})
	want := []scope.Scope{{Account: "111122223333", Region: "eu-west-1"}, {Account: "444455556666", Region: "us-east-1"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("scopesIn() = %v, want %v", got, want)
	}
}

func TestRestoreBuildsStacksForScopesWithData(t *testing.T) {
	stacks := newTestStacks(t)
	sc := scope.Scope{Account: "111122223333", Region: "eu-west-1"}
	if err := stacks.platform.meta.Put(scope.Prefix(sc)+"sqs-queues", "orders", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := stacks.Restore(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, st := range stacks.all() {
		found = found || st.scope == sc
	}
	if !found {
		t.Fatalf("Restore did not build a stack for %v", sc)
	}
}
