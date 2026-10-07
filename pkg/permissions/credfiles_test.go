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
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/childenv"
	"github.com/go-steer/core-agent/v2/pkg/config"
)

// countingPrompter allows everything and counts how often it was asked.
type countingPrompter struct{ asked atomic.Int32 }

func (p *countingPrompter) AskApproval(context.Context, PromptRequest) (Decision, error) {
	p.asked.Add(1)
	return DecisionAllowAlways, nil
}

func writeTable(t *testing.T) (dir, table string) {
	t.Helper()
	dir = t.TempDir()
	table = filepath.Join(dir, "users.json")
	if err := os.WriteFile(table, []byte(`{"version":1,"users":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, table
}

// An out-of-scope read is something an operator may approve; a
// credential file is not. A prompter that would allow anything is never
// even asked, in every mode, for read and write alike.
func TestCredentialFileIsRefusedInEveryModeWithoutAPrompt(t *testing.T) {
	t.Parallel()
	_, table := writeTable(t)
	for _, mode := range []Mode{ModeAsk, ModeAllow, ModeYolo, ModeAcceptEdits, ModePlan} {
		p := &countingPrompter{}
		// A root of "/" puts the table in scope, so nothing about the
		// refusal can be an out-of-scope escalation.
		scope, _ := NewPathScope("/", "", nil)
		g := New(Options{Mode: mode, Scope: scope, Prompter: p})
		g.ProtectCredentialFiles(table)
		ctx := context.Background()
		if err := g.CheckFileRead(ctx, "read_file", table); !errors.Is(err, ErrCredentialFile) {
			t.Errorf("%s: read: want ErrCredentialFile, got %v", mode, err)
		}
		if err := g.CheckFileWrite(ctx, "write_file", table); !errors.Is(err, ErrCredentialFile) {
			t.Errorf("%s: write: want ErrCredentialFile, got %v", mode, err)
		}
		if err := g.CheckBash(ctx, "cat "+table); !errors.Is(err, ErrCredentialFile) {
			t.Errorf("%s: bash: want ErrCredentialFile, got %v", mode, err)
		}
		if n := p.asked.Load(); n != 0 {
			t.Errorf("%s: the operator was asked %d time(s) about the daemon's credential file", mode, n)
		}
	}
}

// The set is shared by reference: a path registered on the template
// after a session gate was derived still covers that session, and a
// derived gate never drops it.
func TestCredentialFilesReachEveryDerivedGate(t *testing.T) {
	t.Parallel()
	_, table := writeTable(t)
	template := New(Options{Mode: ModeYolo})
	early := template.DeriveForSession("early", nil)
	template.ProtectCredentialFiles(table)
	late := template.DeriveForSession("late", nil)
	for name, g := range map[string]*Gate{"early": early, "late": late} {
		if err := g.CheckFileRead(context.Background(), "read_file", table); !errors.Is(err, ErrCredentialFile) {
			t.Errorf("%s session: want ErrCredentialFile, got %v", name, err)
		}
	}
	if got := late.CredentialFiles(); !slices.Equal(got, []string{table}) {
		t.Errorf("CredentialFiles() = %v, want [%s]", got, table)
	}
}

func TestFromConfigProtectsTheBearerTable(t *testing.T) {
	t.Parallel()
	_, table := writeTable(t)
	cfg := config.DefaultConfig()
	cfg.Attach.MultiSession.Auth.TableFile = table
	g, err := FromConfig(cfg, "/", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.CheckFileRead(context.Background(), "read_file", table); !errors.Is(err, ErrCredentialFile) {
		t.Errorf("a gate built from a config naming the bearer table does not protect it: %v", err)
	}
	// And the instruction loader, which takes no gate, is told too — so
	// a library host that builds its gate here gets the @include refusal.
	if !childenv.WithheldFile(table) {
		t.Errorf("FromConfig did not register the bearer table with the process-wide set the instruction loader consults")
	}
}

// The bash check matches the direct spellings of the path as whole shell
// words, including a path relative to the bash tool's working directory.
func TestCredentialBashMentionPattern(t *testing.T) {
	t.Parallel()
	// The spellings childenv.FileSet.Spellings yields for a table at
	// /srv/app/secrets/users.json, resolving to /srv/app/..data/users.json,
	// with the bash tool running in /srv/app.
	pat := bashMentionPattern([]string{"/srv/app/secrets/users.json", "/srv/app/..data/users.json", "secrets/users.json", "..data/users.json"})
	for _, cmd := range []string{
		"cat /srv/app/secrets/users.json",
		"cat '/srv/app/secrets/users.json'",
		`jq . "/srv/app/secrets/users.json"`,
		"cat /srv/app/..data/users.json",
		"cat secrets/users.json",
		"cat ./secrets/users.json",
		"wc -c <secrets/users.json",
		"x=$(cat secrets/users.json)",
		"true;cat secrets/users.json|head",
		"cp secrets/users.json /tmp/x",
	} {
		if !pat.MatchString(cmd) {
			t.Errorf("does not catch %q", cmd)
		}
	}
	for _, cmd := range []string{
		"cat testdata/secrets/users.json",     // a different file
		"cat /srv/app/secrets/users.json.bak", // a different file
		"cat /x/srv/app/secrets/users.json",   // a different file
		"ls /srv/app/secrets",                 // names the directory only
	} {
		if pat.MatchString(cmd) {
			t.Errorf("false positive on %q", cmd)
		}
	}
}

// Pinned so nobody mistakes the bash check for a boundary: a shell can
// name the file in ways no pattern over the command text sees. The
// boundary for bash is the operating system, which is why
// cmd/core-agent warns when the file stays readable by the daemon's
// user. If one of these starts being refused, update the design doc's
// "enforced vs warned" table rather than deleting the case.
func TestCredentialBashCheckIsASeatbeltNotABoundary(t *testing.T) {
	t.Parallel()
	dir, table := writeTable(t)
	g := New(Options{Mode: ModeYolo})
	g.ProtectCredentialFiles(table)
	for _, cmd := range []string{
		"cd " + dir + " && cat users.json",
		"cat " + dir + "/users.*",
		"f=" + dir + "/users; cat ${f}.json",
	} {
		if err := g.CheckBash(context.Background(), cmd); err != nil {
			t.Errorf("%q is now refused (%v) — good, but update docs/credential-files-design.md's table", cmd, err)
		}
	}
}

func TestProtectCredentialFilesIgnoresEmptyAndDuplicates(t *testing.T) {
	t.Parallel()
	_, table := writeTable(t)
	g := New(Options{})
	g.ProtectCredentialFiles("", table, table)
	if got := g.CredentialFiles(); len(got) != 1 {
		t.Errorf("CredentialFiles() = %v, want exactly one entry", got)
	}
	if err := New(Options{Mode: ModeYolo}).CheckFileRead(context.Background(), "read_file", table); err != nil {
		t.Errorf("a gate with no credential files refused a read: %v", err)
	}
}

// An absent table under a symlinked parent (macOS /tmp, a relative
// table_file under a symlinked $PWD). The file tools hand the gate
// ResolvePath(path), which resolves through the deepest existing
// ancestor. A matcher whose EvalSymlinks failed on the missing file and
// fell back to the lexical path matched neither string and had no inode
// to compare, so write_file could create the table, yolo or not.
func TestAbsentCredentialFileUnderASymlinkedParentIsRefused(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	table := filepath.Join(link, "users.json") // absent
	g := New(Options{Mode: ModeYolo})
	g.ProtectCredentialFiles(table)
	resolved, err := ResolvePath(table)
	if err != nil {
		t.Fatal(err)
	}
	if resolved == table {
		t.Fatalf("control: ResolvePath did not resolve the symlinked parent (%s), so this proves nothing", resolved)
	}
	for _, p := range []string{table, resolved} {
		if err := g.CheckFileWrite(context.Background(), "write_file", p); !errors.Is(err, ErrCredentialFile) {
			t.Errorf("CheckFileWrite(%s) on an absent table: want ErrCredentialFile, got %v", p, err)
		}
	}
}
