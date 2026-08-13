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

package subagentlog

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"testing"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// fakeStream is an eventlog.Stream that only answers branch queries —
// enough to exercise Resolve, which never reads events. Since/Watch
// panic so a test that starts depending on them fails loudly rather
// than silently asserting against an empty log.
type fakeStream struct {
	branches []string
	err      error
}

func (f *fakeStream) Append(context.Context, session.Session, *session.Event) (int64, error) {
	panic("unused")
}
func (f *fakeStream) Since(context.Context, int64, ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	panic("unused")
}
func (f *fakeStream) Watch(context.Context, int64, ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	panic("unused")
}
func (f *fakeStream) Close() error { return nil }

func (f *fakeStream) Branches(context.Context, ...eventlog.QueryOption) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return slices.Clone(f.branches), nil
}

// blindStream is a Stream with no BranchLister — the pre-#694 shape.
// The embedded nil interface supplies the Stream methods (and panics
// if one is ever called) without supplying Branches.
type blindStream struct{ eventlog.Stream }

func resolve(t *testing.T, s eventlog.Stream, name string, roster Roster) Query {
	t.Helper()
	return Resolve(context.Background(), s, Tree{AppName: "app", UserID: "u", SessionID: "s"}, name, roster)
}

// TestStripInstanceSuffix pins which suffixes are an instance counter
// and which are part of the declared name — the line between
// resolving "cluster" to "bg.cluster-1" and silently folding a
// distinct "kube-platform" into a query for "kube".
func TestStripInstanceSuffix(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"cluster":       "cluster",
		"cluster-1":     "cluster",
		"cluster-12":    "cluster",
		"cluster-probe": "cluster-probe",
		"kube-platform": "kube-platform",
		"cluster-1a":    "cluster-1a",
		"cluster-":      "cluster-",
		"-1":            "-1",
		"a-1-2":         "a-1",
	} {
		if got := StripInstanceSuffix(in); got != want {
			t.Errorf("StripInstanceSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSplitBranchLabel checks the launch prefix comes off and only the
// top-level segment is kept, whichever separator the runner used.
func TestSplitBranchLabel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ branch, launch, label string }{
		{"cluster", "", "cluster"},
		{"bg.cluster-1", "bg.", "cluster-1"},
		{"bg.cluster-1.bg.probe", "bg.", "cluster-1"},
		{"sub.audit", "sub.", "audit"},
		{"remote.edge/child", "remote.", "edge"},
		{"bgx.cluster", "", "bgx"},
	} {
		launch, label := SplitBranchLabel(tc.branch)
		if launch != tc.launch || label != tc.label {
			t.Errorf("SplitBranchLabel(%q) = (%q, %q), want (%q, %q)",
				tc.branch, launch, label, tc.launch, tc.label)
		}
	}
}

// TestParseLimit verifies an over-large page size is clamped instead
// of honored — the read must not be a lever for dumping the whole log
// in one request.
func TestParseLimit(t *testing.T) {
	t.Parallel()
	if got := ParseLimit("999999"); got != MaxLimit {
		t.Errorf("ParseLimit(999999) = %d, want %d", got, MaxLimit)
	}
	for _, in := range []string{"", "0", "-3", "abc"} {
		if got := ParseLimit(in); got != DefaultLimit {
			t.Errorf("ParseLimit(%q) = %d, want default %d", in, got, DefaultLimit)
		}
	}
	if got := ParseLimit(" 7 "); got != 7 {
		t.Errorf("ParseLimit(%q) = %d, want 7", " 7 ", got)
	}
}

// TestResolve_DeclaredNameMatchesSpawnedInstance is #694's first
// defect at the unit level: a subagent declared as "cluster" writes on
// "bg.cluster-1", which matches none of the four base spellings, so
// the instance-suffixed label has to be added from the log's own
// branches.
func TestResolve_DeclaredNameMatchesSpawnedInstance(t *testing.T) {
	t.Parallel()
	q := resolve(t, &fakeStream{branches: []string{"bg.cluster-1", "sub.audit"}}, "cluster", Roster{})
	if !q.Known {
		t.Fatalf("Resolve(cluster).Known = false, want true")
	}
	if !slices.Contains(q.Prefixes, "bg.cluster-1") {
		t.Errorf("prefixes %v missing bg.cluster-1", q.Prefixes)
	}
	if want := []string{"audit", "cluster"}; !slices.Equal(q.Available, want) {
		t.Errorf("Available = %v, want %v", q.Available, want)
	}
}

// TestResolve_UnknownNameIsNotKnown is #694's second defect: absence
// the log can actually observe must be reported as absence, with the
// names that would have worked.
func TestResolve_UnknownNameIsNotKnown(t *testing.T) {
	t.Parallel()
	q := resolve(t, &fakeStream{branches: []string{"bg.cluster-1"}}, "clustr", Roster{})
	if q.Known {
		t.Fatalf("Resolve(clustr).Known = true, want false")
	}
	if want := []string{"cluster"}; !slices.Equal(q.Available, want) {
		t.Errorf("Available = %v, want %v", q.Available, want)
	}
}

// TestResolve_RosterWidensWithoutTurns pins that a name is real before
// it has written anything: a spawned instance mid-startup and a
// configured-but-never-run subagent both earn an empty page, not a
// not-found.
func TestResolve_RosterWidensWithoutTurns(t *testing.T) {
	t.Parallel()
	s := &fakeStream{}
	if q := resolve(t, s, "cluster", Roster{Instances: []string{"cluster-2"}}); !q.Known {
		t.Errorf("live instance cluster-2 did not make %q known", "cluster")
	}
	if q := resolve(t, s, "auditor", Roster{Declared: []string{"auditor"}}); !q.Known {
		t.Errorf("configured auditor did not make the name known")
	}
	if q := resolve(t, s, "ghost", Roster{Declared: []string{"auditor"}}); q.Known {
		t.Errorf("ghost resolved against a roster that doesn't list it")
	}
}

// TestResolve_UnprovableAbsenceDegradesToKnown covers the three ways
// the branch scan can fail to see everything. In each, absence was not
// observed, so claiming it would turn a working subagent into a
// not-found: the honest answer is an empty page plus a logged reason.
//
// The capped-scan case is the sharp one — the scan returns the store's
// first branchScanLimit labels, so a name sorting past the cap looks
// missing when it is merely late.
func TestResolve_UnprovableAbsenceDegradesToKnown(t *testing.T) {
	t.Parallel()
	capped := make([]string, branchScanLimit)
	for i := range capped {
		capped[i] = fmt.Sprintf("bg.filler-%03d", i)
	}
	for _, tc := range []struct {
		name   string
		stream eventlog.Stream
	}{
		{"no BranchLister", &blindStream{}},
		{"scan failed", &fakeStream{err: errors.New("db is on fire")}},
		{"scan hit the cap", &fakeStream{branches: capped}},
	} {
		q := resolve(t, tc.stream, "nowhere", Roster{})
		if !q.Known {
			t.Errorf("%s: Known = false, want true (absence was not observed)", tc.name)
		}
		if q.ResolveErr == nil {
			t.Errorf("%s: ResolveErr = nil, want a logged reason", tc.name)
		}
	}
}

// TestResolve_BaseSpellingsAlwaysQueried guards the union: a sync
// subagent tags its events with the bare name and the background
// manager with "bg.", and the operator doesn't know which ran.
func TestResolve_BaseSpellingsAlwaysQueried(t *testing.T) {
	t.Parallel()
	q := resolve(t, &fakeStream{}, "auditor", Roster{})
	for _, want := range []string{"auditor", "bg.auditor", "sub.auditor", "remote.auditor"} {
		if !slices.Contains(q.Prefixes, want) {
			t.Errorf("prefixes %v missing %q", q.Prefixes, want)
		}
	}
}

// TestValidateName rejects query keys that could never have produced a
// branch label, so a name can't smuggle separators into the filter.
func TestValidateName(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", " lead", "trail ", "a.b", "a/b", "a b"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", bad)
		}
	}
	for _, ok := range []string{"cluster", "cluster-1", "kube_probe"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", ok, err)
		}
	}
}
