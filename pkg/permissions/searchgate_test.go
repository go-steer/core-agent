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

package permissions

import (
	"context"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// TestSearchShapedCommand_TheObservedSession runs the three calls the
// #158 session actually made in its first four tool calls. All three
// must be caught: the first set the trajectory for 164 turns and $5.41
// with no diagnosis.
func TestSearchShapedCommand_TheObservedSession(t *testing.T) {
	observed := []struct {
		command string
		want    string
	}{
		{`grep -rn "Session stats" .`, "grep"},
		{`grep -ir "Session stats" ./*.go ./pkg ./cmd ./internal ./runner ./usage`, "grep"},
		{`find . -name "*.go" | xargs grep -i "Session stats"`, "find"},
	}
	for _, tc := range observed {
		got, native, ok := SearchShapedCommand(tc.command)
		if !ok {
			t.Errorf("SearchShapedCommand(%q) = not search-shaped; this is the exact call #158 is about", tc.command)
			continue
		}
		if got != tc.want {
			t.Errorf("SearchShapedCommand(%q) binary = %q, want %q", tc.command, got, tc.want)
		}
		if native == "" {
			t.Errorf("SearchShapedCommand(%q) named no replacement tool; a refusal with no alternative is a dead end", tc.command)
		}
	}
}

func TestSearchShapedCommand_Refuses(t *testing.T) {
	cases := map[string]string{
		"grep -rn foo .":                     "grep",
		"rg --json 'func main'":              "rg",
		"ag foo":                             "ag",
		"ack foo":                            "ack",
		"egrep -r foo .":                     "egrep",
		"fgrep foo file.go":                  "fgrep",
		"fd -e go":                           "fd",
		"find . -name '*.go'":                "find",
		"/usr/bin/grep -rn foo .":            "grep",
		"grep foo file.go | head -20":        "grep",
		"make build && grep -rn TODO .":      "grep",
		"cd /tmp; grep -rn foo .":            "grep",
		"(grep -rn foo .)":                   "grep",
		"grep -rn foo . || echo none":        "grep",
		"grep foo < input.txt":               "grep",
		"GREP_COLORS=never grep -rn foo src": "grep",
	}
	for command, want := range cases {
		got, _, ok := SearchShapedCommand(command)
		if !ok {
			t.Errorf("SearchShapedCommand(%q) = allowed, want refused (%s)", command, want)
			continue
		}
		if got != want {
			t.Errorf("SearchShapedCommand(%q) = %q, want %q", command, got, want)
		}
	}
}

// TestSearchShapedCommand_AllowsStreamFilters is the load-bearing
// negative case. `go test ./... | grep -v ok` filters a stream; the
// native grep tool searches files and cannot do it, so refusing here
// would be refusing something with no replacement — and a gate that
// blocks legitimate work gets set to "allow" and stops mattering.
func TestSearchShapedCommand_AllowsStreamFilters(t *testing.T) {
	allowed := []string{
		"go test ./... | grep -v ok",
		"git log --oneline | grep fix",
		"kubectl get pods | grep CrashLoop",
		"cat log.txt | grep ERROR | wc -l",
		"docker ps | grep core-agent | awk '{print $1}'",
		"ps aux |& grep core-agent",
	}
	for _, command := range allowed {
		if binary, _, ok := SearchShapedCommand(command); ok {
			t.Errorf("SearchShapedCommand(%q) refused %q; a piped search filters a stream and has no native equivalent", command, binary)
		}
	}
}

// TestSearchShapedCommand_AllowsNonSearchWork asserts the gate is
// surgical. Everything bash is legitimately for must survive it —
// that is the entire argument for this over --disable-tools=bash.
func TestSearchShapedCommand_AllowsNonSearchWork(t *testing.T) {
	allowed := []string{
		"go test ./pkg/foo/",
		"go build ./...",
		"make test",
		"git status",
		"git log --grep=fix --oneline",
		"gofmt -l .",
		"npm install",
		"echo grep",
		"ls -la",
		"cat findings.md",
		"./scripts/find-stuff.sh",
		"go run ./cmd/core-agent -p 'find the bug'",
	}
	for _, command := range allowed {
		if binary, _, ok := SearchShapedCommand(command); ok {
			t.Errorf("SearchShapedCommand(%q) refused on %q, but this is not a search command", command, binary)
		}
	}
}

// TestSearchShapedCommand_FindWithActionIsNotASearch covers the one
// carve-out: with an action predicate, find is a file operation and
// `glob` cannot stand in for it.
func TestSearchShapedCommand_FindWithActionIsNotASearch(t *testing.T) {
	operations := []string{
		"find . -name '*.tmp' -delete",
		"find . -name '*.go' -exec gofmt -w {} ;",
		"find /tmp -type f -execdir rm {} +",
	}
	for _, command := range operations {
		if _, _, ok := SearchShapedCommand(command); ok {
			t.Errorf("SearchShapedCommand(%q) refused; find with an action predicate is an operation, not a lookup", command)
		}
	}
	// The same find without the action IS a search.
	if _, _, ok := SearchShapedCommand("find . -name '*.tmp'"); !ok {
		t.Error("find without an action predicate should still be refused")
	}
}

// TestSearchShapedCommand_FailsOpen documents the deliberate limit: the
// gate steers a model, it does not contain an adversary. Anything it
// cannot read literally is allowed rather than guessed at, because a
// false refusal on a real build command costs more than a missed nudge.
func TestSearchShapedCommand_FailsOpen(t *testing.T) {
	unresolvable := []string{
		"$TOOL -rn foo .",
		"eval 'grep -rn foo .'",
		"sh -c 'grep -rn foo .'",
		"if true; then grep -rn foo .; fi",
	}
	for _, command := range unresolvable {
		if _, _, ok := SearchShapedCommand(command); ok {
			t.Errorf("SearchShapedCommand(%q) refused; the gate is documented to fail open here", command)
		}
	}
}

func TestSearchGateMessage_IsActionable(t *testing.T) {
	msg := SearchGateMessage("grep", "grep")
	for _, want := range []string{"`grep`", "native", "bash_search_gate"} {
		if !strings.Contains(msg, want) {
			t.Errorf("SearchGateMessage missing %q; the model needs the replacement and the operator needs the override: %s", want, msg)
		}
	}
	if glob := SearchGateMessage("find", "glob"); !strings.Contains(glob, "glob") {
		t.Errorf("find should be routed to glob, got: %s", glob)
	}
}

// --- gate integration ---

func newSearchGate(t *testing.T, mode string) *Gate {
	t.Helper()
	return New(Options{Mode: ModeYolo, BashSearchGate: mode})
}

func TestCheckBash_SearchGateModes(t *testing.T) {
	ctx := context.Background()

	t.Run("enforce refuses", func(t *testing.T) {
		err := newSearchGate(t, config.BashSearchGateEnforce).CheckBash(ctx, "grep -rn foo .")
		if err == nil {
			t.Fatal("enforce mode allowed a bash grep")
		}
		if !strings.Contains(err.Error(), "native `grep` tool") {
			t.Errorf("error must name the replacement, got: %v", err)
		}
	})

	t.Run("enforce beats yolo", func(t *testing.T) {
		// The gate is in yolo, which approves everything else. The
		// search gate runs as a pre-check for the same reason the
		// denylist does: a posture the operator configured should not
		// be waved through by the permission mode.
		g := newSearchGate(t, config.BashSearchGateEnforce)
		if err := g.CheckBash(ctx, "go test ./..."); err != nil {
			t.Fatalf("yolo should allow a normal command: %v", err)
		}
		if err := g.CheckBash(ctx, "grep -rn foo ."); err == nil {
			t.Error("yolo waved through a search-shaped command")
		}
	})

	t.Run("warn allows and advises", func(t *testing.T) {
		g := newSearchGate(t, config.BashSearchGateWarn)
		if err := g.CheckBash(ctx, "grep -rn foo ."); err != nil {
			t.Fatalf("warn mode must not refuse: %v", err)
		}
		notice := g.BashSearchNotice(ctx, "grep -rn foo .")
		if notice == "" {
			t.Fatal("warn mode produced no notice; then it is just allow with extra steps")
		}
		if !strings.Contains(notice, "native `grep` tool") {
			t.Errorf("notice must name the replacement, got: %s", notice)
		}
	})

	t.Run("allow disables the check", func(t *testing.T) {
		g := newSearchGate(t, config.BashSearchGateAllow)
		if err := g.CheckBash(ctx, "grep -rn foo ."); err != nil {
			t.Fatalf("allow mode refused: %v", err)
		}
		if n := g.BashSearchNotice(ctx, "grep -rn foo ."); n != "" {
			t.Errorf("allow mode emitted a notice: %s", n)
		}
	})

	t.Run("notice is empty outside warn mode", func(t *testing.T) {
		// Enforce already refused, so a notice would be dead text; a
		// non-search command has nothing to say in any mode.
		if n := newSearchGate(t, config.BashSearchGateEnforce).BashSearchNotice(ctx, "grep -rn foo ."); n != "" {
			t.Errorf("enforce mode emitted a notice: %s", n)
		}
		if n := newSearchGate(t, config.BashSearchGateWarn).BashSearchNotice(ctx, "go test ./..."); n != "" {
			t.Errorf("non-search command produced a notice: %s", n)
		}
	})
}

// TestCheckBash_SearchGateDefaultsToEnforce pins the default. An
// opt-in guardrail nobody opts into is the shipped-but-inert pattern
// this milestone exists to remove.
func TestCheckBash_SearchGateDefaultsToEnforce(t *testing.T) {
	g := New(Options{Mode: ModeYolo}) // no BashSearchGate set
	if got := g.BashSearchGate(); got != config.BashSearchGateEnforce {
		t.Errorf("default posture = %q, want %q", got, config.BashSearchGateEnforce)
	}
	if err := g.CheckBash(context.Background(), "grep -rn foo ."); err == nil {
		t.Error("default gate allowed a bash grep")
	}
}

// TestDeriveForSession_InheritsSearchGate is the multi-session hole:
// a sub-gate that defaulted its posture instead of inheriting it would
// leave the gate off for every daemon-created session while the boot
// line still claimed enforce.
func TestDeriveForSession_InheritsSearchGate(t *testing.T) {
	for _, mode := range []string{config.BashSearchGateEnforce, config.BashSearchGateWarn, config.BashSearchGateAllow} {
		sub := newSearchGate(t, mode).DeriveForSession("sess-1", nil)
		if got := sub.BashSearchGate(); got != mode {
			t.Errorf("sub-gate posture = %q, want inherited %q", got, mode)
		}
	}

	// And the behavior, not just the field: an "allow" template must
	// not produce a sub-gate that refuses.
	sub := newSearchGate(t, config.BashSearchGateAllow).DeriveForSession("sess-2", nil)
	if err := sub.CheckBash(context.Background(), "grep -rn foo ."); err != nil {
		t.Errorf("sub-gate of an allow template refused: %v", err)
	}
}

// TestCheckBash_NoRefusalWithoutAReplacement is the inverse of the bug
// this milestone is about: a refusal that names a tool the model can't
// call is a claim the runtime doesn't back. A build that dropped `grep`
// from the catalog and kept `bash` must not be told to use `grep`.
func TestCheckBash_NoRefusalWithoutAReplacement(t *testing.T) {
	ctx := context.Background()

	t.Run("grep dropped, glob kept", func(t *testing.T) {
		g := newSearchGate(t, config.BashSearchGateEnforce)
		g.SetNativeSearchTools(map[string]bool{"grep": false, "glob": true})
		if err := g.CheckBash(ctx, "grep -rn foo ."); err != nil {
			t.Errorf("refused a bash grep with no native grep to offer: %v", err)
		}
		// glob is still there, so find is still redirectable.
		if err := g.CheckBash(ctx, "find . -name '*.go'"); err == nil {
			t.Error("find should still be refused while glob is registered")
		}
	})

	t.Run("both dropped is inert", func(t *testing.T) {
		g := newSearchGate(t, config.BashSearchGateEnforce)
		g.SetNativeSearchTools(map[string]bool{})
		for _, cmd := range []string{"grep -rn foo .", "rg foo", "find . -name x", "fd -e go"} {
			if err := g.CheckBash(ctx, cmd); err != nil {
				t.Errorf("CheckBash(%q) refused with no native tools registered: %v", cmd, err)
			}
		}
		if got := g.ActiveSearchBinaries(); len(got) != 0 {
			t.Errorf("ActiveSearchBinaries = %v, want empty so the boot line can report INERT", got)
		}
	})

	t.Run("warn mode advice is suppressed too", func(t *testing.T) {
		g := newSearchGate(t, config.BashSearchGateWarn)
		g.SetNativeSearchTools(map[string]bool{"grep": false, "glob": false})
		if n := g.BashSearchNotice(ctx, "grep -rn foo ."); n != "" {
			t.Errorf("notice = %q, want empty — it points at a tool that isn't registered", n)
		}
	})

	t.Run("unset means assume registered", func(t *testing.T) {
		// A host wiring tools by hand never calls SetNativeSearchTools;
		// that must keep the gate, not silently disarm it.
		g := newSearchGate(t, config.BashSearchGateEnforce)
		if err := g.CheckBash(ctx, "grep -rn foo ."); err == nil {
			t.Error("gate disarmed itself when the native-tool set was never supplied")
		}
		if got := len(g.ActiveSearchBinaries()); got != len(SearchGatedBinaries()) {
			t.Errorf("ActiveSearchBinaries = %d entries, want all %d", got, len(SearchGatedBinaries()))
		}
	})

	t.Run("sub-gates inherit the registered set", func(t *testing.T) {
		tmpl := newSearchGate(t, config.BashSearchGateEnforce)
		tmpl.SetNativeSearchTools(map[string]bool{"grep": false, "glob": false})
		if err := tmpl.DeriveForSession("s", nil).CheckBash(ctx, "grep -rn foo ."); err != nil {
			t.Errorf("sub-gate refused where its template would not: %v", err)
		}
	})
}

// TestSearchGate_FollowsTheContextSessionGate covers the multi-session
// daemon shape: the bash tool is built once and closes over the daemon
// gate, while the decision belongs to the per-session sub-gate carried
// on the context. Both the refusal and the warn-mode notice have to
// read the same gate, or one session's posture leaks into another's.
func TestSearchGate_FollowsTheContextSessionGate(t *testing.T) {
	daemon := newSearchGate(t, config.BashSearchGateEnforce)
	session := New(Options{Mode: ModeYolo, BashSearchGate: config.BashSearchGateWarn})
	ctx := WithSessionGate(context.Background(), session)

	if err := daemon.CheckBash(ctx, "grep -rn foo ."); err != nil {
		t.Errorf("daemon gate refused where the session gate is in warn mode: %v", err)
	}
	if n := daemon.BashSearchNotice(ctx, "grep -rn foo ."); n == "" {
		t.Error("no notice: the advice came from the daemon gate, not the session gate that allowed the call")
	}
}

// TestCheckBash_DenylistStillRunsFirst guards ordering: the search gate
// must not shadow the destructive-command denylist.
func TestCheckBash_DenylistStillRunsFirst(t *testing.T) {
	g := newSearchGate(t, config.BashSearchGateEnforce)
	err := g.CheckBash(context.Background(), "rm -rf /")
	if err == nil {
		t.Fatal("denylist did not fire")
	}
	if strings.Contains(err.Error(), "search-shaped") {
		t.Errorf("search gate shadowed the denylist reason: %v", err)
	}
}
