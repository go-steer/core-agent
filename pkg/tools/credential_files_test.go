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

package tools

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/agent"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

const bearerTableSecret = "tok-1201-bearer-table-secret"

// bearerTableFixture writes a users.json and a harmless sibling into a
// directory that is the gate's project root — the worst placement, in
// scope for every file tool — and returns a yolo gate built the way the
// daemon builds it, from a config naming that table. Yolo is the point:
// nothing about the refusal may depend on there being an operator.
func bearerTableFixture(t *testing.T) (dir, table, sibling string, gate *permissions.Gate) {
	t.Helper()
	dir = t.TempDir()
	table = filepath.Join(dir, "users.json")
	body := `{"version":1,"users":[{"identity":"alice","token":"` + bearerTableSecret + `"}]}`
	if err := os.WriteFile(table, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling = filepath.Join(dir, "notes.json")
	if err := os.WriteFile(sibling, []byte(`{"note":"ordinary"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Permissions.Mode = string(permissions.ModeYolo)
	cfg.Attach.MultiSession.Auth.TableFile = table
	template, err := permissions.FromConfig(cfg, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The agent runs against a derived gate, as in the daemon.
	return dir, table, sibling, template.DeriveForSession("s1", nil)
}

func mustNotLeak(t *testing.T, route string, res any, err error) {
	t.Helper()
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), bearerTableSecret) {
		t.Errorf("%s: the agent read the bearer table's token: %s", route, b)
	}
	if err != nil && strings.Contains(err.Error(), bearerTableSecret) {
		t.Errorf("%s: the token surfaced in the error: %v", route, err)
	}
}

func mustRefuse(t *testing.T, route string, res any, err error) {
	t.Helper()
	mustNotLeak(t, route, res, err)
	if !errors.Is(err, permissions.ErrCredentialFile) {
		t.Errorf("%s: want a credential-file refusal, got err=%v", route, err)
	}
}

// #1201 item 4: the multi-session bearer table is the credential that
// answers the agent's permission prompts, and the agent runs as the
// daemon's user, so file modes cannot keep it out. Every file tool that
// opens a path must be refused on it — reading it would let the agent
// approve its own calls, writing it would let it add its own token — and
// so must the agent's own bash call when it names the file.
func TestAgentToolsCannotReachTheBearerTable(t *testing.T) {
	t.Parallel()
	dir, table, sibling, gate := bearerTableFixture(t)
	cfg := config.DefaultConfig()
	ctx := adkagent.ToolContext(nil)

	// Control: the same tools read an ordinary file beside it, so every
	// refusal below is about the table, not the directory or the mode.
	if res, err := readFileFunc(gate, cfg)(ctx, readFileArgs{Path: sibling}); err != nil || !strings.Contains(res.Content, "ordinary") {
		t.Fatalf("control: read_file of a sibling failed (%v), so the refusals below prove nothing", err)
	}
	if res, err := bashFunc(gate, cfg)(ctx, bashArgs{Command: "cat " + sibling}); err != nil || !strings.Contains(res.Stdout, "ordinary") {
		t.Fatalf("control: bash cat of a sibling failed (%v), so the refusals below prove nothing", err)
	}

	r1, err := readFileFunc(gate, cfg)(ctx, readFileArgs{Path: table})
	mustRefuse(t, "read_file", r1, err)
	r2, err := bashFunc(gate, cfg)(ctx, bashArgs{Command: "cat " + table})
	mustRefuse(t, "bash cat <abs>", r2, err)
	r3, err := bashFunc(gate, cfg)(ctx, bashArgs{Command: `jq -r '.users[].token' "` + table + `"`})
	mustRefuse(t, "bash jq <quoted abs>", r3, err)
	r4, err := jsonQueryFunc(gate, cfg)(ctx, jsonQueryArgs{Path: table, Query: ".users[].token"})
	mustRefuse(t, "json_query", r4, err)
	r5, err := viewFileOutlineFunc(gate, cfg)(ctx, viewFileOutlineArgs{Path: table})
	mustRefuse(t, "view_file_outline", r5, err)
	r6, err := grepFunc(gate, cfg)(ctx, grepArgs{Path: table, Pattern: "tok"})
	mustRefuse(t, "grep <table>", r6, err)

	// Directory walks skip it silently rather than failing the walk.
	r7, err := grepFunc(gate, cfg)(ctx, grepArgs{Path: dir, Pattern: "tok|ordinary"})
	mustNotLeak(t, "grep <dir>", r7, err)
	if err != nil || !strings.Contains(mustJSON(r7), "ordinary") {
		t.Errorf("grep <dir>: the walk should still search the sibling (err=%v): %s", err, mustJSON(r7))
	}
	r8, err := readManyFilesFunc(gate, cfg)(ctx, readManyFilesArgs{Path: dir, Pattern: "*.json"})
	mustNotLeak(t, "read_many_files <pattern>", r8, err)
	r9, err := readManyFilesFunc(gate, cfg)(ctx, readManyFilesArgs{Paths: []string{table}})
	mustNotLeak(t, "read_many_files <paths>", r9, err)

	// Writes: an agent that can rewrite the table can add its own token
	// for the next boot.
	w1, err := writeFileFunc(gate)(ctx, writeFileArgs{Path: table, Content: "{}"})
	mustRefuse(t, "write_file", w1, err)
	w2, err := editFileFunc(gate)(ctx, editFileArgs{Path: table, OldString: "alice", NewString: "agent"})
	mustRefuse(t, "edit_file", w2, err)
	w3, err := deleteFileFunc(gate)(ctx, deleteFileArgs{Path: table})
	mustRefuse(t, "delete_file", w3, err)
	if got, _ := os.ReadFile(table); !strings.Contains(string(got), bearerTableSecret) || !strings.Contains(string(got), "alice") {
		t.Errorf("the bearer table was modified through an agent tool: %q", got)
	}
}

// The other spellings a file tool follows at the OS level: a symlink the
// agent planted and a hard link it made. Path comparison alone misses
// the hard link, which is why the gate also compares inodes.
func TestAgentToolsCannotReachTheBearerTableThroughALink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink and hard link semantics")
	}
	dir, table, _, gate := bearerTableFixture(t)
	cfg := config.DefaultConfig()
	ctx := adkagent.ToolContext(nil)

	sym := filepath.Join(dir, "innocent.txt")
	if err := os.Symlink(table, sym); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(dir, "also-innocent.txt")
	if err := os.Link(table, hard); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{sym, hard} {
		res, err := readFileFunc(gate, cfg)(ctx, readFileArgs{Path: p})
		mustRefuse(t, "read_file "+filepath.Base(p), res, err)
	}
}

// A Kubernetes Secret volume serves users.json as a symlink into
// ..data, itself a symlink into a timestamped directory. On every Secret
// update kubelet writes a new directory, re-points ..data and deletes the
// old one, so the resolved path and the inode the gate saw at boot both
// go stale. read_file resolves symlinks before it asks the gate, so a
// gate that compared only what it recorded at boot would wave the new
// file through on its ordinary path.
func TestAgentToolsCannotReachTheBearerTableAfterASecretUpdate(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics")
	}
	dir := t.TempDir()
	writeGen := func(gen, token string) {
		t.Helper()
		if err := os.Mkdir(filepath.Join(dir, gen), 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"version":1,"users":[{"identity":"alice","token":"` + token + `"}]}`
		if err := os.WriteFile(filepath.Join(dir, gen, "users.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeGen("..2026_A", bearerTableSecret)
	if err := os.Symlink("..2026_A", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	table := filepath.Join(dir, "users.json")
	if err := os.Symlink("..data/users.json", table); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Permissions.Mode = string(permissions.ModeYolo)
	cfg.Attach.MultiSession.Auth.TableFile = table
	template, err := permissions.FromConfig(cfg, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	gate := template.DeriveForSession("s1", nil)

	// kubelet's update: new generation, atomic swap of ..data, old one gone.
	writeGen("..2026_B", bearerTableSecret+"-rotated")
	tmp := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink("..2026_B", tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "..2026_A")); err != nil {
		t.Fatal(err)
	}

	ctx := adkagent.ToolContext(nil)
	for _, p := range []string{table, filepath.Join(dir, "..2026_B", "users.json"), filepath.Join(dir, "..data", "users.json")} {
		res, err := readFileFunc(gate, config.DefaultConfig())(ctx, readFileArgs{Path: p})
		mustRefuse(t, "read_file "+strings.TrimPrefix(p, dir), res, err)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
