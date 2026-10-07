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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/childenv"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

const (
	tableE2EChildFlag  = "CORE_AGENT_1201_TABLE_E2E_CHILD"
	tableE2EOutVar     = "CORE_AGENT_1201_TABLE_E2E_OUT"
	tableE2ESecret     = "tok-1201-e2e-bearer"
	tableE2EIncludeVar = "CORE_AGENT_1201_TABLE_E2E_INCLUDE"
)

// An agent with only write_file — no bash at all — could otherwise add
// `@include users.json` to AGENTS.md and receive every bearer token in
// the next session's system prompt. run() must register the table with
// the instruction loader before its boot-time load; this drives the real
// main() with that AGENTS.md in place. The scripted provider echoes
// nothing, so the assertion is on the daemon's refusal: it names the
// table, never prints the token, and stops before any turn runs.
func TestRunRefusesToIncludeTheBearerTableInAPrompt(t *testing.T) {
	if os.Getenv(tableE2EChildFlag) == "1" {
		tableE2EChild()
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh")
	}
	t.Parallel()
	dir := t.TempDir()
	out := filepath.Join(dir, "seen")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunRefusesToIncludeTheBearerTableInAPrompt$", "-test.count=1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), tableE2EChildFlag+"=1", tableE2EOutVar+"="+out, tableE2EIncludeVar+"=1")
	cmd.Stdin = strings.NewReader("")
	combined, _ := cmd.CombinedOutput()
	if !strings.Contains(string(combined), "is one of the daemon's credential files and is never loaded into a prompt") {
		t.Errorf("the boot-time instruction load did not refuse an @include of the bearer table:\n%s", combined)
	}
	if strings.Contains(string(combined), tableE2ESecret) {
		t.Errorf("the token surfaced in the daemon's output:\n%s", combined)
	}
	if _, err := os.Stat(out); err == nil {
		t.Errorf("a turn ran with the bearer table spliced into its prompt")
	}
}

// The pkg/tools and pkg/permissions tests prove a gate built from a
// config naming the bearer table refuses every route to it. This proves
// the daemon builds its gate that way: real flag parsing, real run(),
// the real bash tool, under --yolo, with a scripted model whose second
// bash call cats the table by its absolute path. The first call is the
// control that bash ran at all.
func TestRunRefusesTheAgentsBashOnTheBearerTable(t *testing.T) {
	if os.Getenv(tableE2EChildFlag) == "1" {
		tableE2EChild()
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh")
	}
	t.Parallel()
	dir := t.TempDir()
	out := filepath.Join(dir, "seen")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunRefusesTheAgentsBashOnTheBearerTable$", "-test.count=1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), tableE2EChildFlag+"=1", tableE2EOutVar+"="+out)
	cmd.Stdin = strings.NewReader("")
	combined, err := cmd.CombinedOutput()
	seen, readErr := os.ReadFile(out)
	if readErr != nil || !strings.Contains(string(seen), "RAN") {
		t.Fatalf("the control bash call never ran, so nothing was tested (child err=%v, read err=%v):\n%s", err, readErr, combined)
	}
	if strings.Contains(string(seen), tableE2ESecret) {
		t.Errorf("the agent's bash read the multi-session bearer table under a daemon whose config names it:\n%s\n--- daemon output ---\n%s", seen, combined)
	}
	for _, want := range []string{
		"agent tools may not read or write",
		"is readable by the user this daemon runs as, and so by the agent's bash",
	} {
		if !strings.Contains(string(combined), want) {
			t.Errorf("startup did not say %q:\n%s", want, combined)
		}
	}
}

func tableE2EChild() {
	dir, _ := os.Getwd()
	out := os.Getenv(tableE2EOutVar)
	table := filepath.Join(dir, "users.json")
	_ = os.WriteFile(table, []byte(`{"version":1,"users":[{"identity":"alice","token":"`+tableE2ESecret+`"}]}`+"\n"), 0o600)
	call := func(command string) map[string]any {
		return map[string]any{"responses": []any{map[string]any{
			"Content":      map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "bash", "args": map[string]any{"command": command}}}}},
			"TurnComplete": true, "FinishReason": "STOP",
		}}}
	}
	lines := []map[string]any{
		call(`printf 'RAN\n' > "` + out + `"`),
		call(`cat ` + table + ` >> "` + out + `"`),
		{"responses": []any{map[string]any{
			"Content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": "done"}}},
			"TurnComplete": true, "FinishReason": "STOP",
		}}},
	}
	var script bytes.Buffer
	for _, l := range lines {
		b, _ := json.Marshal(l)
		script.Write(append(b, '\n'))
	}
	scriptPath := filepath.Join(dir, "script.jsonl")
	cfgPath := filepath.Join(dir, ".agents", "config.json")
	_ = os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	_ = os.WriteFile(scriptPath, script.Bytes(), 0o644)
	cfg := `{"attach":{"multi_session":{"auth":{"table_file":"` + table + `"}}}}` + "\n"
	_ = os.WriteFile(cfgPath, []byte(cfg), 0o644)
	if os.Getenv(tableE2EIncludeVar) == "1" {
		// What an agent holding only write_file could leave behind.
		_ = os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Project notes.\n\n@include users.json\n"), 0o644)
	}
	os.Args = []string{"core-agent", "-c", cfgPath, "--provider=scripted", "--script", scriptPath, "--yolo", "-p", "go"}
	main()
	os.Exit(0)
}

const (
	tokenE2EChildFlag = "CORE_AGENT_1201_TOKEN_E2E_CHILD" // "config" or "flag"
	tokenE2ESecret    = "tok-1201-e2e-attach-token-file"
)

// The attach token file (#1266) answers prompts exactly as the bearer
// table does, and it reaches run() two ways: attach.token_file in config,
// which FromConfig sees, and --attach-token-file, which lands only in
// attachCfg. Both must be refused to the agent's bash and to @include.
func TestRunProtectsTheAttachTokenFile(t *testing.T) {
	if mode := os.Getenv(tokenE2EChildFlag); mode != "" {
		tokenE2EChild(mode)
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh")
	}
	t.Parallel()
	for _, mode := range []string{"config", "flag"} {
		for _, include := range []bool{false, true} {
			name := mode + "/bash"
			if include {
				name = mode + "/include"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				out := filepath.Join(dir, "seen")
				env := append(os.Environ(), tokenE2EChildFlag+"="+mode, tableE2EOutVar+"="+out)
				if include {
					env = append(env, tableE2EIncludeVar+"=1")
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestRunProtectsTheAttachTokenFile$", "-test.count=1")
				cmd.Dir = dir
				cmd.Env = env
				cmd.Stdin = strings.NewReader("")
				combined, _ := cmd.CombinedOutput()
				seen, _ := os.ReadFile(out)
				if strings.Contains(string(seen)+string(combined), tokenE2ESecret) {
					t.Fatalf("the attach token file's contents reached the agent or the output:\n%s\n--- daemon ---\n%s", seen, combined)
				}
				if include {
					if !strings.Contains(string(combined), "is one of the daemon's credential files and is never loaded into a prompt") {
						t.Errorf("an @include of the attach token file was not refused:\n%s", combined)
					}
					return
				}
				if !strings.Contains(string(seen), "RAN") {
					t.Fatalf("the control bash call never ran, so nothing was tested:\n%s", combined)
				}
				if !strings.Contains(string(combined), "agent tools may not read or write") || !strings.Contains(string(combined), "attach-token") {
					t.Errorf("startup did not list the attach token file as protected:\n%s", combined)
				}
			})
		}
	}
}

func tokenE2EChild(mode string) {
	dir, _ := os.Getwd()
	out := os.Getenv(tableE2EOutVar)
	tokenFile := filepath.Join(dir, "attach-token")
	_ = os.WriteFile(tokenFile, []byte(tokenE2ESecret+"\n"), 0o600)
	call := func(command string) map[string]any {
		return map[string]any{"responses": []any{map[string]any{
			"Content":      map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "bash", "args": map[string]any{"command": command}}}}},
			"TurnComplete": true, "FinishReason": "STOP",
		}}}
	}
	lines := []map[string]any{
		call(`printf 'RAN\n' > "` + out + `"`),
		call(`cat ` + tokenFile + ` >> "` + out + `"`),
		{"responses": []any{map[string]any{
			"Content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": "done"}}},
			"TurnComplete": true, "FinishReason": "STOP",
		}}},
	}
	var script bytes.Buffer
	for _, l := range lines {
		b, _ := json.Marshal(l)
		script.Write(append(b, '\n'))
	}
	scriptPath := filepath.Join(dir, "script.jsonl")
	cfgPath := filepath.Join(dir, ".agents", "config.json")
	_ = os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	_ = os.WriteFile(scriptPath, script.Bytes(), 0o644)
	cfg := "{}\n"
	args := []string{"core-agent", "-c", cfgPath, "--provider=scripted", "--script", scriptPath, "--yolo"}
	if mode == "config" {
		cfg = `{"attach":{"token_file":"` + tokenFile + `"}}` + "\n"
	} else {
		args = append(args, "--attach-token-file", tokenFile)
	}
	_ = os.WriteFile(cfgPath, []byte(cfg), 0o644)
	if os.Getenv(tableE2EIncludeVar) == "1" {
		_ = os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Project notes.\n\n@include attach-token\n"), 0o644)
	}
	args = append(args, "-p", "go")
	os.Args = args
	main()
	os.Exit(0)
}

// guardCredentialFiles registers the flag-only token file on the gate it
// is handed, so a read_file of it is refused like the bearer table's.
func TestGuardCredentialFilesProtectsTheFlagTokenFile(t *testing.T) {
	t.Parallel()
	tokenFile := filepath.Join(t.TempDir(), "attach-token")
	if err := os.WriteFile(tokenFile, []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	template := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	session := template.DeriveForSession("s1", nil)
	var protected int
	var stderr bytes.Buffer
	guardCredentialFilesWith(template, tokenFile, false, &stderr, fakeProbes(false, childenv.PtraceCaps{}, nil, &protected))
	if err := session.CheckFileRead(context.Background(), "read_file", tokenFile); !errors.Is(err, permissions.ErrCredentialFile) {
		t.Errorf("read_file of the --attach-token-file path: want ErrCredentialFile, got %v", err)
	}
	if protected != 1 {
		t.Errorf("protect called %d time(s), want 1", protected)
	}
}

func TestCredentialFileExposure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                    string
		readable, bash, inScope bool
		want                    []string // substrings; nil means no warning
	}{
		{"unreadable: nothing the agent can reach", false, true, true, nil},
		{"no bash, outside scope: the gate covers every route", true, false, false, nil},
		{"bash, outside scope", true, true, false, []string{"so by the agent's bash", "Disable the bash tool", "#1201"}},
		{"no bash, inside scope", true, false, true, []string{"inside the agent's path scope", "mount it outside"}},
		{"bash, inside scope", true, true, true, []string{"so by the agent's bash", "It also sits inside the agent's path scope"}},
	}
	for _, c := range cases {
		got := credentialFileExposure("/etc/core-agent/users.json", c.readable, c.bash, c.inScope)
		if c.want == nil {
			if got != "" {
				t.Errorf("%s: unexpected warning %q", c.name, got)
			}
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: warning %q lacks %q", c.name, got, w)
			}
		}
	}
}

func fakeProbes(root bool, caps childenv.PtraceCaps, protectErr error, protected *int) credentialProbes {
	return credentialProbes{
		readable:  func(string) bool { return true },
		root:      func() bool { return root },
		caps:      func() (childenv.PtraceCaps, error) { return caps, nil },
		protect:   func() error { *protected++; return protectErr },
		supported: true,
	}
}

// A daemon that read a credential file holds it in memory, so it is made
// non-dumpable; one that read none is not, and pays nothing.
func TestGuardCredentialFilesProtectsOnlyWhenAFileIsHeld(t *testing.T) {
	t.Parallel()
	table := filepath.Join(t.TempDir(), "users.json")
	var protected int
	var stderr bytes.Buffer
	g := permissions.New(permissions.Options{})
	guardCredentialFilesWith(g, "", false, &stderr, fakeProbes(false, childenv.PtraceCaps{}, nil, &protected))
	if protected != 0 || stderr.Len() != 0 {
		t.Errorf("no credential files: protect called %d time(s), output %q", protected, stderr.String())
	}
	g.ProtectCredentialFiles(table)
	guardCredentialFilesWith(g, "", false, &stderr, fakeProbes(false, childenv.PtraceCaps{}, errors.New("EINVAL"), &protected))
	if protected != 1 {
		t.Errorf("credential file held: protect called %d time(s), want 1", protected)
	}
	if !strings.Contains(stderr.String(), "could not make the daemon non-dumpable (EINVAL)") {
		t.Errorf("a failed protect was not reported:\n%s", stderr.String())
	}
}

// #1201 item 5 end to end through the startup function: a root daemon
// with CAP_SYS_PTRACE in its bounding set that holds the bearer table is
// told so; the same daemon with the capability dropped is not.
func TestGuardCredentialFilesWarnsARootPtraceDaemon(t *testing.T) {
	t.Parallel()
	g := permissions.New(permissions.Options{})
	g.ProtectCredentialFiles(filepath.Join(t.TempDir(), "users.json"))
	var protected int
	var stderr bytes.Buffer
	guardCredentialFilesWith(g, "", false, &stderr, fakeProbes(true, childenv.PtraceCaps{Bounding: true}, nil, &protected))
	if !strings.Contains(stderr.String(), "runs as root with CAP_SYS_PTRACE") {
		t.Errorf("root + CAP_SYS_PTRACE holding the bearer table was not warned:\n%s", stderr.String())
	}
	stderr.Reset()
	guardCredentialFilesWith(g, "", false, &stderr, fakeProbes(true, childenv.PtraceCaps{}, nil, &protected))
	if strings.Contains(stderr.String(), "CAP_SYS_PTRACE") {
		t.Errorf("warned a root daemon whose bounding set lacks CAP_SYS_PTRACE:\n%s", stderr.String())
	}
}
