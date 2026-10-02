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
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/childenv"
	"github.com/go-steer/core-agent/v2/pkg/config"
)

const (
	e2eChildFlag  = "CORE_AGENT_1157_E2E_CHILD"
	e2eTokenVar   = "CORE_AGENT_1157_E2E_ATTACH_TOKEN"
	e2eSecret     = "s3cret-1157-e2e"
	e2eOutFileVar = "CORE_AGENT_1157_E2E_OUT"
)

// The package tests prove each exec site scrubs once a name is
// withheld; this proves run() withholds it at all. Deleting the
// withholdDaemonCredentials call from run() leaves every one of those
// green — the defect would be in WHEN, which no unit test of a helper
// can see (AGENTS.md's #647 pitfall).
//
// So this drives the real binary's main(): real flag parsing, real
// run(), the real bash tool, a scripted model that issues one bash call.
// No build step — the test binary for package main already contains
// main(), so a re-exec'd child sets os.Args and calls it.
//
// The bash command writes what it saw to a file, because -p prints the
// model's final text, not tool output.
func TestRunWithholdsTheAttachTokenFromTheAgentsBash(t *testing.T) {
	if os.Getenv(e2eChildFlag) == "1" {
		e2eChild()
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("the /proc half of this property is Linux-only")
	}
	t.Parallel()
	dir := t.TempDir()
	out := filepath.Join(dir, "seen")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunWithholdsTheAttachTokenFromTheAgentsBash$", "-test.count=1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), e2eChildFlag+"=1", e2eTokenVar+"="+e2eSecret, e2eOutFileVar+"="+out)
	cmd.Stdin = strings.NewReader("")
	combined, err := cmd.CombinedOutput()
	seen, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("the scripted bash call never ran, so nothing was tested (child err=%v):\n%s", err, combined)
	}
	got := string(seen)
	if !strings.Contains(got, "RAN") {
		t.Fatalf("bash ran but wrote no marker:\n%s", got)
	}
	if strings.Contains(got, e2eSecret) {
		t.Errorf("a daemon started with --attach-token=%s handed the token to the agent's bash:\n%s\n--- daemon output ---\n%s", e2eTokenVar, got, combined)
	}
	if !strings.Contains(string(combined), "child processes do not inherit "+e2eTokenVar) {
		t.Errorf("startup did not report the withheld name:\n%s", combined)
	}
}

// e2eChild is the daemon: a scripted model whose first turn calls bash
// and whose second ends the run.
func e2eChild() {
	dir, _ := os.Getwd()
	out := os.Getenv(e2eOutFileVar)
	probe := `printf 'RAN\n' > "` + out + `"; ` +
		`printf 'INHERITED=%s\n' "$` + e2eTokenVar + `" >> "` + out + `"; ` +
		`tr '\0' '\n' < /proc/$PPID/environ 2>/dev/null | grep '^` + e2eTokenVar + `=' >> "` + out + `" || true`
	lines := []map[string]any{
		{"responses": []any{map[string]any{
			"Content":      map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "bash", "args": map[string]any{"command": probe}}}}},
			"TurnComplete": true, "FinishReason": "STOP",
		}}},
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
	_ = os.WriteFile(cfgPath, []byte("{}\n"), 0o644)
	os.Args = []string{"core-agent", "-c", cfgPath, "--provider=scripted", "--script", scriptPath,
		"--yolo", "--attach-token", e2eTokenVar, "-p", "go"}
	main()
	os.Exit(0)
}

// The withheld set is the daemon's by-name credentials: every *_env
// config field, plus the attach token, whose name can come from the
// --attach-token flag with no config behind it. Missing the flag case
// would leave the one credential #1157 is about inheritable on exactly
// the invocation shape the issue describes.
//
// None of the variables is set, so HoldsCredential is false and the
// shared test process is never made non-dumpable; that half is driven
// end to end by pkg/tools' TestBashCannotReachTheDaemonsAttachToken.
func TestDaemonWithholdsConfigCredentialsAndTheFlagOnlyAttachToken(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.Alerts.Targets = []config.AlertTarget{{
		Name:   "ops",
		URLEnv: "CORE_AGENT_1157_CFG_WEBHOOK_URL",
		Auth:   &config.AlertAuth{BearerEnv: "CORE_AGENT_1157_CFG_BEARER"},
	}}
	var stderr bytes.Buffer
	withholdDaemonCredentials(cfg, "CORE_AGENT_1157_FLAG_TOKEN", &stderr)

	got := childenv.Withheld()
	for _, want := range []string{
		"CORE_AGENT_1157_CFG_WEBHOOK_URL", // a Slack webhook URL is the secret
		"CORE_AGENT_1157_CFG_BEARER",
		"CORE_AGENT_1157_FLAG_TOKEN",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not withheld from child processes (withheld: %v)", want, got)
		}
	}
	if !strings.Contains(stderr.String(), "child processes do not inherit") {
		t.Errorf("startup does not report what is withheld, so an operator whose hook relied on one of these cannot find out why it stopped seeing it:\n%s", stderr.String())
	}
}
