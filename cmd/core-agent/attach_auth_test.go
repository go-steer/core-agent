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
	"errors"
	"flag"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/config"
)

// ---- unit: the listener policy ---------------------------------------

func TestCheckLocalListenerAuth(t *testing.T) {
	t.Parallel()
	loopback := attach.Options{Addr: "127.0.0.1:7777"}
	socket := attach.Options{UnixSocket: "/tmp/agent.sock"}
	cases := []struct {
		name      string
		opts      attach.Options
		shell     bool
		allow     bool
		wantErr   bool
		wantWarn  bool
		errSubstr []string
	}{
		{name: "loopback, no auth, bash registered: refused", opts: loopback, shell: true, wantErr: true,
			errSubstr: []string{"refusing to start the attach listener on 127.0.0.1:7777", "--attach-token=", "--attach-token-file", "--attach-readonly", "--attach-allow-unauthenticated-local", "--disable-tools=bash", "perms/respond"}},
		{name: "unix socket, no auth, bash registered: refused", opts: socket, shell: true, wantErr: true,
			errSubstr: []string{"unix socket /tmp/agent.sock"}},
		{name: "no bash: the #376 posture stands", opts: loopback, shell: false},
		{name: "explicit opt-out: warns, starts", opts: loopback, shell: true, allow: true, wantWarn: true},
		{name: "bearer token", opts: attach.Options{Addr: "127.0.0.1:7777", Auth: attach.AuthConfig{BearerToken: "t"}}, shell: true},
		{name: "mTLS", opts: attach.Options{UnixSocket: "/s", Auth: attach.AuthConfig{ClientCAFile: "ca.pem", TLSCertFile: "tls.crt", TLSKeyFile: "tls.key"}}, shell: true},
		{name: "a client CA without TLS gates nothing", opts: attach.Options{Addr: "127.0.0.1:7777", Auth: attach.AuthConfig{ClientCAFile: "ca.pem"}}, shell: true, wantErr: true},
		{name: "read-only: the agent's bash cannot write through it", opts: attach.Options{Addr: "127.0.0.1:7777", Auth: attach.AuthConfig{ReadOnly: true}}, shell: true},
		{name: "enforced multi-session", opts: attach.Options{Addr: "127.0.0.1:7777", MultiSessionEnabled: true}, shell: true},
		{name: "multi-session with anonymous fallback is no gate", opts: attach.Options{Addr: "127.0.0.1:7777", MultiSessionEnabled: true, AllowAnonymous: true}, shell: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			err := checkLocalListenerAuth(tc.opts, tc.shell, tc.allow, &stderr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			for _, s := range tc.errSubstr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("refusal does not mention %q, so the operator is not told how to fix it:\n%v", s, err)
				}
			}
			if gotWarn := strings.Contains(stderr.String(), "warning: --attach-allow-unauthenticated-local"); gotWarn != tc.wantWarn {
				t.Errorf("warning printed = %v, want %v:\n%s", gotWarn, tc.wantWarn, stderr.String())
			}
		})
	}
}

// ---- unit: the daemon's token resolution -----------------------------

func writeTokenFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // umask-proof
		t.Fatal(err)
	}
	return p
}

// Not parallel: t.Setenv. The file cases call TakeFile, which makes this
// test process non-dumpable; that has no effect on other tests.
func TestResolveAttachToken(t *testing.T) {
	t.Setenv("CORE_AGENT_1201_RESOLVE_TOKEN", "from-env")
	file := writeTokenFile(t, "from-file\n", 0o600)

	got, err := resolveAttachToken(attachOpts{TokenFile: file}, &bytes.Buffer{})
	if err != nil || got != "from-file" {
		t.Errorf("token file: got %q, %v; want from-file (trailing newline trimmed)", got, err)
	}
	got, err = resolveAttachToken(attachOpts{TokenEnv: "CORE_AGENT_1201_RESOLVE_TOKEN"}, &bytes.Buffer{})
	if err != nil || got != "from-env" {
		t.Errorf("token env: got %q, %v; want from-env", got, err)
	}
	got, err = resolveAttachToken(attachOpts{TokenEnv: "CORE_AGENT_1201_RESOLVE_UNSET"}, &bytes.Buffer{})
	if err != nil || got != "" {
		t.Errorf("unset env: got %q, %v; want \"\" and no error (the listener block refuses it; hub registration tolerates it)", got, err)
	}
	if _, err = resolveAttachToken(attachOpts{TokenFile: file, TokenEnv: "CORE_AGENT_1201_RESOLVE_TOKEN"}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("both sources: err = %v, want mutually exclusive", err)
	}
	loose := writeTokenFile(t, "t\n", 0o644)
	if _, err = resolveAttachToken(attachOpts{TokenFile: loose}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "--attach-token-file") {
		t.Errorf("world-readable file: err = %v, want a --attach-token-file refusal", err)
	}
	if _, err = resolveAttachToken(attachOpts{TokenFile: filepath.Join(t.TempDir(), "missing")}, &bytes.Buffer{}); err == nil {
		t.Error("a missing token file resolved without error")
	}
}

// A config attach.token_file on a daemon with no listener and no hub
// registration is never read, so `core-agent -p` cannot fail on it or
// block opening a FIFO nobody writes to.
func TestAttachTokenIfUsedSkipsDaemonsThatNeverPresentIt(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	if tok, err := attachTokenIfUsed(attachOpts{TokenFile: missing}, &bytes.Buffer{}); err != nil || tok != "" {
		t.Errorf("no listener: got %q, %v; want the file left unread", tok, err)
	}
	for _, o := range []attachOpts{
		{TokenFile: missing, Listen: "127.0.0.1:7777"},
		{TokenFile: missing, UnixSocket: "/tmp/s.sock"},
		{TokenFile: missing, RegisterTo: "https://hub:7777"},
	} {
		if _, err := attachTokenIfUsed(o, &bytes.Buffer{}); err == nil {
			t.Errorf("%+v: a missing token file was not read", o)
		}
	}
}

// ---- unit: CLI-beats-config treats the two token sources as one ------

func TestMergeAttachOpts_TokenSourcesOverrideAsOneSetting(t *testing.T) {
	t.Parallel()
	parse := func(t *testing.T, args ...string) (*flag.FlagSet, *attachOpts) {
		t.Helper()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		o := registerAttachFlags(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return fs, o
	}
	t.Run("CLI file replaces config env", func(t *testing.T) {
		t.Parallel()
		fs, o := parse(t, "-attach-token-file=/run/secrets/attach")
		got := mergeAttachOpts(*o, config.AttachConfig{TokenEnv: "CFG_TOKEN"}, fs)
		if got.TokenFile != "/run/secrets/attach" || got.TokenEnv != "" {
			t.Errorf("got TokenFile=%q TokenEnv=%q; a CLI token file must replace the config's token_env, not collide with it", got.TokenFile, got.TokenEnv)
		}
	})
	t.Run("CLI env replaces config file", func(t *testing.T) {
		t.Parallel()
		fs, o := parse(t, "-attach-token=CLI_TOKEN")
		got := mergeAttachOpts(*o, config.AttachConfig{TokenFile: "/cfg/token"}, fs)
		if got.TokenEnv != "CLI_TOKEN" || got.TokenFile != "" {
			t.Errorf("got TokenFile=%q TokenEnv=%q", got.TokenFile, got.TokenEnv)
		}
	})
	t.Run("config alone flows through", func(t *testing.T) {
		t.Parallel()
		fs, o := parse(t)
		got := mergeAttachOpts(*o, config.AttachConfig{TokenFile: "/cfg/token"}, fs)
		if got.TokenFile != "/cfg/token" {
			t.Errorf("got TokenFile=%q, want the config value", got.TokenFile)
		}
	})
	t.Run("both on the CLI stay both, and resolve refuses them", func(t *testing.T) {
		t.Parallel()
		fs, o := parse(t, "-attach-token=CLI_TOKEN", "-attach-token-file=/x")
		got := mergeAttachOpts(*o, config.AttachConfig{}, fs)
		if _, err := resolveAttachToken(got, &bytes.Buffer{}); err == nil {
			t.Error("--attach-token and --attach-token-file together resolved without error")
		}
	})
}

// ---- e2e: the real binary, the real bash tool ------------------------

const (
	authE2EChildFlag = "CORE_AGENT_1201_AUTH_E2E_CHILD"
	authE2EMode      = "CORE_AGENT_1201_AUTH_E2E_MODE"
	authE2EPort      = "CORE_AGENT_1201_AUTH_E2E_PORT"
	authE2EProbe     = "CORE_AGENT_1201_AUTH_E2E_PROBE"
	authE2EOut       = "CORE_AGENT_1201_AUTH_E2E_OUT"
	authE2ETokenFile = "CORE_AGENT_1201_AUTH_E2E_TOKEN_FILE"
	authE2ESecret    = "s3cret-1201-token-file"
)

// authProbe is what the agent's bash runs: everything a same-user shell
// can try for the attach credential, then the listener itself with no
// credential. $1 is the daemon's pid, embedded by the daemon when it
// builds its script, because /bin/sh -c may or may not fork before
// exec'ing bash and $PPID would then name the wrong process.
//
// What it finds goes to one file per source ($out.env, $out.environ,
// $out.fifo), which the test searches but never prints: they hold the
// test runner's whole environment, and CI logs a failing test's output.
// $out itself is a printable summary.
const authProbe = `#!/usr/bin/env bash
pid="$1"; port="$2"; out="$3"; fifo="$4"
{
  echo RAN
  env > "$out.env"
  if tr '\0' '\n' < "/proc/$pid/environ" > "$out.environ" 2>/dev/null; then
    echo "ENVIRON=readable"
  else
    echo "ENVIRON=denied"
  fi
  if [ -n "$fifo" ]; then timeout 2 cat "$fifo" > "$out.fifo" 2>/dev/null; echo "FIFO_BYTES=$(wc -c < "$out.fifo")"; fi
  if exec 3<>"/dev/tcp/127.0.0.1/$port"; then
    printf 'GET /sessions HTTP/1.0\r\nHost: 127.0.0.1\r\n\r\n' >&3
    echo "HTTP=$(head -n1 <&3)"
    exec 3>&-
  else
    echo "HTTP=connect-failed"
  fi
} > "$out" 2>&1
`

type authE2EResult struct {
	exitCode int
	stderr   string
	probe    string // "" when the agent's bash never ran
	// found names each source in which the probe found the token
	// ("env", "environ", "fifo"); the sources themselves are not kept.
	found []string
}

// runAuthE2E re-execs this test binary as a daemon (see e2eChild in
// credentials_test.go for why no build step is needed) with an attach
// listener on a free loopback port and a scripted model whose one turn
// runs authProbe through the bash tool.
func runAuthE2E(t *testing.T, testName, mode string, extraEnv ...string) authE2EResult {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("probes /proc and bash's /dev/tcp")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(probe, []byte(authProbe), 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "seen")
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$", "-test.count=1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), authE2EChildFlag+"=1", authE2EMode+"="+mode,
		authE2EPort+"="+strconv.Itoa(freePort(t)), authE2EProbe+"="+probe, authE2EOut+"="+out)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin = strings.NewReader("")
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := authE2EResult{stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		res.exitCode = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("daemon did not run: %v", err)
	}
	if b, rerr := os.ReadFile(out); rerr == nil {
		res.probe = string(b)
	}
	for _, src := range []string{"env", "environ", "fifo"} {
		if b, rerr := os.ReadFile(out + "." + src); rerr == nil && bytes.Contains(b, []byte(authE2ESecret)) {
			res.found = append(res.found, src)
		}
	}
	return res
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// authE2EChild is the daemon side.
func authE2EChild() {
	dir, _ := os.Getwd()
	mode := os.Getenv(authE2EMode)
	port := os.Getenv(authE2EPort)
	tokenFile := os.Getenv(authE2ETokenFile)
	command := strings.Join([]string{"bash", os.Getenv(authE2EProbe), strconv.Itoa(os.Getpid()), port, os.Getenv(authE2EOut), tokenFile}, " ")
	lines := []map[string]any{
		{"responses": []any{map[string]any{
			"Content":      map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "bash", "args": map[string]any{"command": command}}}}},
			"TurnComplete": true, "FinishReason": "STOP",
		}}},
		{"responses": []any{map[string]any{
			"Content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": "done"}}},
			"TurnComplete": true, "FinishReason": "STOP",
		}}},
	}
	if mode == "nobash" {
		lines = lines[1:]
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
	os.Args = []string{"core-agent", "-c", cfgPath, "--provider=scripted", "--script", scriptPath, "--yolo",
		"--session-db-path", filepath.Join(dir, "sessions.db"), "--attach-listen", "127.0.0.1:" + port, "-p", "go"}
	switch mode {
	case "allow":
		os.Args = append(os.Args, "--attach-allow-unauthenticated-local")
	case "tokenfile":
		os.Args = append(os.Args, "--attach-token-file", tokenFile)
	case "nobash":
		os.Args = append(os.Args, "--disable-tools=bash")
	}
	main()
	os.Exit(0)
}

// #1201 item 2. A token-less loopback listener on a daemon whose agent
// has bash used to start with a log warning, and the agent's bash could
// then answer its own permission prompts over it with no credential at
// all. The daemon now refuses to start, before any turn runs, so the
// agent's bash never gets the chance to try.
func TestRunRefusesATokenlessLocalListenerWhenTheAgentHasBash(t *testing.T) {
	if os.Getenv(authE2EChildFlag) == "1" {
		authE2EChild()
		return
	}
	t.Parallel()
	res := runAuthE2E(t, "TestRunRefusesATokenlessLocalListenerWhenTheAgentHasBash", "refuse")
	if res.exitCode != 2 {
		t.Errorf("exit code %d, want 2 (config error)", res.exitCode)
	}
	if !strings.Contains(res.stderr, "refusing to start the attach listener on 127.0.0.1:") {
		t.Errorf("startup did not refuse the token-less listener:\n%s", res.stderr)
	}
	if res.probe != "" {
		t.Errorf("a turn ran and the agent's bash reached the listener:\n%s", res.probe)
	}
}

// The control for the refusal: with the operator's explicit opt-out the
// daemon starts, and the agent's own bash reaches the listener with no
// credential (HTTP 200). This is the route the refusal closes; without
// this case the refusal test could pass on a daemon whose bash could not
// reach the listener anyway.
func TestRunWithTheOptOutTheAgentsBashReachesTheListener(t *testing.T) {
	if os.Getenv(authE2EChildFlag) == "1" {
		authE2EChild()
		return
	}
	t.Parallel()
	res := runAuthE2E(t, "TestRunWithTheOptOutTheAgentsBashReachesTheListener", "allow")
	if res.exitCode != 0 {
		t.Fatalf("exit code %d, want 0:\n%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "warning: --attach-allow-unauthenticated-local") {
		t.Errorf("the opt-out started silently:\n%s", res.stderr)
	}
	if !strings.Contains(res.probe, "HTTP=HTTP/1.1 200") && !strings.Contains(res.probe, "HTTP=HTTP/1.0 200") {
		t.Errorf("the control failed: the agent's bash did not reach the unauthenticated listener, so the refusal test proves nothing:\n%s\n--- daemon ---\n%s", res.probe, res.stderr)
	}
}

// Without bash the #376 posture stands: the listener starts token-less.
// Pins that the refusal reads the registered catalog, not the default.
func TestRunStartsATokenlessLocalListenerWithoutBash(t *testing.T) {
	if os.Getenv(authE2EChildFlag) == "1" {
		authE2EChild()
		return
	}
	t.Parallel()
	res := runAuthE2E(t, "TestRunStartsATokenlessLocalListenerWithoutBash", "nobash")
	if res.exitCode != 0 || !strings.Contains(res.stderr, "attach listener on 127.0.0.1:") {
		t.Errorf("exit %d; a daemon without bash should start its token-less loopback listener:\n%s", res.exitCode, res.stderr)
	}
}

// #1201 item 3, daemon half. With --attach-token-file the token never
// enters any environment, so the agent's bash finds it nowhere: not
// inherited, not in the daemon's /proc environ, and — because
// the file here is a FIFO, the posture the docs recommend — not in the
// file either, which the daemon's single read consumed. The listener it
// reaches without the token answers 401.
func TestRunWithAnAttachTokenFileTheAgentsBashFindsNoToken(t *testing.T) {
	if os.Getenv(authE2EChildFlag) == "1" {
		authE2EChild()
		return
	}
	t.Parallel()
	fifo := filepath.Join(t.TempDir(), "attach-token")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	// The writer blocks until the daemon opens the FIFO, writes once and
	// closes; after that nobody holds the write end, so a later reader
	// (the probe) gets nothing.
	wrote := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			wrote <- err
			return
		}
		_, err = f.WriteString(authE2ESecret + "\n")
		_ = f.Close()
		wrote <- err
	}()
	res := runAuthE2E(t, "TestRunWithAnAttachTokenFileTheAgentsBashFindsNoToken", "tokenfile", authE2ETokenFile+"="+fifo)
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("writing the token into the FIFO: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon never read the token file")
	}
	if res.exitCode != 0 {
		t.Fatalf("exit code %d:\n%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.probe, "RAN") {
		t.Fatalf("the agent's bash never ran, so nothing was tested:\n%s", res.stderr)
	}
	t.Logf("what the agent's bash saw:\n%s", res.probe)
	if len(res.found) > 0 {
		t.Errorf("the agent's bash found the attach token in: %s", strings.Join(res.found, ", "))
	}
	if !strings.Contains(res.probe, "HTTP=HTTP/1.1 401") && !strings.Contains(res.probe, "HTTP=HTTP/1.0 401") {
		t.Errorf("the listener did not demand the token from the file:\n%s\n--- daemon ---\n%s", res.probe, res.stderr)
	}
	if strings.Contains(res.stderr, authE2ESecret) {
		t.Errorf("the daemon printed the token:\n%s", res.stderr)
	}
}
