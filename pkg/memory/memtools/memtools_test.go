// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package memtools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-agent/v3/internal/testutil"
	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/memory"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

// fakeCtx is the slice of adkagent.Context the handlers touch: the
// context.Context methods and UserID. Anything else panics, which is
// the point — a handler reaching further would be caught here.
type fakeCtx struct {
	adkagent.Context
	ctx context.Context
	uid string
}

func (f fakeCtx) Deadline() (time.Time, bool) { return f.ctx.Deadline() }
func (f fakeCtx) Done() <-chan struct{}       { return f.ctx.Done() }
func (f fakeCtx) Err() error                  { return f.ctx.Err() }
func (f fakeCtx) Value(k any) any             { return f.ctx.Value(k) }
func (f fakeCtx) UserID() string              { return f.uid }

func tctx(uid string) fakeCtx { return fakeCtx{ctx: context.Background(), uid: uid} }

func newRouter(t *testing.T, mounts ...memory.MountTemplate) *memory.Router {
	t.Helper()
	r, err := memory.NewRouter(memory.NewInMemory(), mounts)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

func names(ts []tool.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, tl := range ts {
		out = append(out, tl.Name())
	}
	return out
}

func yolo() *permissions.Gate {
	return permissions.New(permissions.Options{Mode: permissions.ModeYolo})
}

// The sweep (internal/testutil.ModelFacingBans, #909). Built with a
// writable mount so all three tools exist; the read-only shape is
// asserted separately below, so nothing conditional escapes it.
func TestMemoryToolTextIsDeploymentNeutral(t *testing.T) {
	t.Parallel()
	r := newRouter(t,
		memory.AgentMount("prod", "triage", memory.AccessReadWrite),
		memory.SharedMount("prod", "runbooks", memory.AccessReadOnly),
	)
	ts, err := New(r, Options{Gate: yolo()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(ts) != 3 {
		t.Fatalf("swept %d tools (%v), want all 3: the sweep would miss one", len(ts), names(ts))
	}
	for _, tl := range ts {
		texts, scanned := testutil.ModelFacingText(tl)
		if !scanned {
			t.Errorf("tool %q exposes no arg schema to scan", tl.Name())
		}
		for _, text := range texts {
			for _, bad := range testutil.ModelFacingBanViolations(text) {
				t.Errorf("tool %q: %s\n  %s", tl.Name(), bad, text)
			}
		}
		refs, checked := testutil.UndeclaredArgRefs(tl)
		if !checked {
			t.Errorf("tool %q exposes no arg schema to cross-check its description against", tl.Name())
		}
		for _, ref := range refs {
			t.Errorf("tool %q description tells the model to set %q, which it does not declare:\n  %s", tl.Name(), ref, tl.Description())
		}
	}
}

func TestNewRegistersOnlyWhatTheMountsAllow(t *testing.T) {
	t.Parallel()
	if ts, err := New(nil, Options{}); err != nil || len(ts) != 0 {
		t.Fatalf("nil router: tools=%v err=%v, want none", names(ts), err)
	}
	if ts, err := New(newRouter(t), Options{}); err != nil || len(ts) != 0 {
		t.Fatalf("no mounts: tools=%v err=%v, want none", names(ts), err)
	}
	ro := newRouter(t, memory.SharedMount("prod", "runbooks", memory.AccessReadOnly))
	ts, err := New(ro, Options{})
	if err != nil {
		t.Fatalf("New(ro): %v", err)
	}
	if got := names(ts); len(got) != 1 || got[0] != RecallToolName {
		t.Fatalf("read-only mounts registered %v, want only %s: a write tool that always fails must not be advertised", got, RecallToolName)
	}
}

func TestPersistRecallForgetRoundTrip(t *testing.T) {
	t.Parallel()
	r := newRouter(t,
		memory.AgentMount("prod", "triage", memory.AccessReadWrite),
		memory.SharedMount("prod", "fleet", memory.AccessReadWrite),
	)
	h := &handlers{router: r, gate: yolo()}
	ctx := tctx("alice")

	got, err := h.persist(ctx, PersistArgs{Fact: "Replica lag alerts fire above 30 seconds", Topics: []string{"db"}})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if got.Mount != memory.MountAgent || len(got.Deltas) != 1 {
		t.Fatalf("persist defaulted to %q with %d deltas, want agent with 1", got.Mount, len(got.Deltas))
	}
	if _, err := h.persist(ctx, PersistArgs{Fact: "Payments runs in us-central1", Mount: "shared:fleet"}); err != nil {
		t.Fatalf("persist shared: %v", err)
	}

	rec, err := h.recall(ctx, RecallArgs{Query: "replica lag"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(rec.Items) == 0 || rec.Items[0].Mount != memory.MountAgent || rec.Items[0].ReadOnly {
		t.Fatalf("recall = %+v, want the agent memory first, tagged writable", rec.Items)
	}
	id := rec.Items[0].ID

	// The id belongs to "agent"; naming the wrong scope must not delete it.
	if _, err := h.forget(ctx, ForgetArgs{ID: id, Mount: "shared:fleet"}); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("forget in the wrong scope: err=%v, want ErrNotFound", err)
	}
	if _, err := h.forget(ctx, ForgetArgs{ID: id, Mount: memory.MountAgent}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	rec, _ = h.recall(ctx, RecallArgs{Query: "replica lag", Mount: memory.MountAgent})
	if len(rec.Items) != 0 {
		t.Fatalf("memory still recalled after forget: %+v", rec.Items)
	}
}

func TestPersistNeedsAMountWhenAgentIsNotWritable(t *testing.T) {
	t.Parallel()
	r := newRouter(t,
		memory.AgentMount("prod", "triage", memory.AccessReadOnly),
		memory.SharedMount("prod", "fleet", memory.AccessReadWrite),
	)
	h := &handlers{router: r}
	_, err := h.persist(tctx("u"), PersistArgs{Fact: "x"})
	if err == nil || !strings.Contains(err.Error(), "shared:fleet") {
		t.Fatalf("persist with no mount and read-only agent: err=%v, want one naming the writable scope", err)
	}
}

// The gate key is the contract operators write policies against:
// "persist_memory:mount=agent *" must allow private writes while a
// shared write in the same session is refused.
func TestGateKeysAreMountQualified(t *testing.T) {
	t.Parallel()
	pol, err := permissions.NewPolicy(
		[]string{"recall_memory:mount=all *", "persist_memory:mount=agent *"},
		[]string{"persist_memory:mount=shared:fleet *"},
	)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeAllow, Policy: pol})
	r := newRouter(t,
		memory.AgentMount("prod", "triage", memory.AccessReadWrite),
		memory.SharedMount("prod", "fleet", memory.AccessReadWrite),
	)
	h := &handlers{router: r, gate: gate}
	ctx := tctx("u")
	if _, err := h.persist(ctx, PersistArgs{Fact: "private note"}); err != nil {
		t.Fatalf("private write under an allow rule for mount=agent: %v", err)
	}
	if _, err := h.persist(ctx, PersistArgs{Fact: "fleet note", Mount: "shared:fleet"}); err == nil {
		t.Fatal("shared write was allowed despite a deny rule for mount=shared:fleet")
	}
	if _, err := h.recall(ctx, RecallArgs{Query: "note"}); err != nil {
		t.Fatalf("recall under an allow rule for mount=all: %v", err)
	}
}

// partition_by_user mounts need a user id. The tools resolve it from
// the verified attach caller first, then the runner's session user.
func TestUserIdentityResolution(t *testing.T) {
	t.Parallel()
	m := memory.SharedMount("prod", "prefs", memory.AccessReadWrite)
	m.PartitionByUser = true
	r := newRouter(t, m)
	h := &handlers{router: r}

	if _, err := h.persist(tctx(""), PersistArgs{Fact: "anonymous", Mount: "shared:prefs"}); !errors.Is(err, memory.ErrMissingUserIdentity) {
		t.Fatalf("no identity anywhere: err=%v, want ErrMissingUserIdentity", err)
	}
	if _, err := h.persist(tctx("bob"), PersistArgs{Fact: "bob prefers tables", Mount: "shared:prefs"}); err != nil {
		t.Fatalf("persist as session user bob: %v", err)
	}
	// A verified caller outranks the session user.
	carol := fakeCtx{ctx: auth.WithCaller(context.Background(), auth.Caller{Identity: "carol"}), uid: "bob"}
	if res, err := h.recall(carol, RecallArgs{Query: "tables"}); err != nil || len(res.Items) != 0 {
		t.Fatalf("carol recalled bob's partition: items=%+v err=%v", res.Items, err)
	}
	if res, err := h.recall(tctx("bob"), RecallArgs{Query: "tables"}); err != nil || len(res.Items) != 1 {
		t.Fatalf("bob lost his own memory: items=%+v err=%v", res.Items, err)
	}
}

func TestTruncateKeepsKeysShortAndValidUTF8(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 200)
	got := truncate(long)
	if len(got) > maxKeyDetail+len("…") {
		t.Fatalf("truncate returned %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "…") || strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("truncate split a rune or lost the marker: %q", got)
	}
	if got := truncate("a\n  b\tc"); got != "a b c" {
		t.Fatalf("truncate did not collapse whitespace: %q", got)
	}
}

// recall_memory across "all" must not reach a scope a per-mount deny
// rule names; the key for the call itself says mount=all, so without
// the filter a deny on mount=shared:secret would never be consulted.
func TestRecallAllSkipsDeniedMounts(t *testing.T) {
	t.Parallel()
	pol, err := permissions.NewPolicy(nil, []string{"recall_memory:mount=shared:secret *"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	r := newRouter(t,
		memory.AgentMount("prod", "triage", memory.AccessReadWrite),
		memory.SharedMount("prod", "secret", memory.AccessReadWrite),
	)
	ctx := tctx("u")
	open := &handlers{router: r}
	if _, err := open.persist(ctx, PersistArgs{Fact: "SECRET replica hint", Mount: "shared:secret"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := &handlers{router: r, gate: permissions.New(permissions.Options{Mode: permissions.ModeYolo, Policy: pol})}
	res, err := h.recall(ctx, RecallArgs{Query: "replica"})
	if err != nil {
		t.Fatalf("recall all: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("recall across all read a denied scope: %+v", res.Items)
	}
	if _, err := h.recall(ctx, RecallArgs{Query: "replica", Mount: "shared:secret"}); err == nil {
		t.Fatal("explicit recall of a denied scope was allowed")
	}
	planGate := permissions.New(permissions.Options{Mode: permissions.ModePlan, Policy: pol})
	if got := PermittedRecallMounts(ctx, planGate, r, "replica"); len(got) != 1 || got[0] != memory.MountAgent {
		t.Fatalf("PermittedRecallMounts in plan mode = %v, want [agent]", got)
	}
}

// A write that landed but whose audit row failed is reported as a
// success with a warning, so the model does not retry and duplicate it.
func TestAuditFailureIsReportedAsAWarningNotAFailure(t *testing.T) {
	t.Parallel()
	r, err := memory.NewRouter(memory.NewInMemory(),
		[]memory.MountTemplate{memory.AgentMount("prod", "triage", memory.AccessReadWrite)},
		memory.WithAuditRecorder(func(context.Context, string, map[string]any) error { return errors.New("audit store down") }))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	h := &handlers{router: r}
	ctx := tctx("u")
	got, err := h.persist(ctx, PersistArgs{Fact: "Payments runs in us-central1"})
	if err != nil || got.Warning == "" || len(got.Deltas) != 1 {
		t.Fatalf("persist with failing audit: res=%+v err=%v, want success with a warning", got, err)
	}
	f, err := h.forget(ctx, ForgetArgs{ID: got.Deltas[0].ID, Mount: memory.MountAgent})
	if err != nil || f.Warning == "" || !f.Deleted {
		t.Fatalf("forget with failing audit: res=%+v err=%v, want success with a warning", f, err)
	}
}
