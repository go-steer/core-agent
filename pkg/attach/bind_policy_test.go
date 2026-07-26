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

package attach

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// Regression tests for #376: the tokenless default previously bound
// all interfaces with zero authorization and no warning — any LAN peer
// could read transcripts, drive the agent, and answer permission
// prompts.

// TestNewServer_RefusesNonLoopbackWithoutAuth verifies the Options
// validation: binding a non-loopback address without any credential
// gate is a construction-time error, not a running open listener.
func TestNewServer_RefusesNonLoopbackWithoutAuth(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	addrs := []string{
		":7777",             // empty host = all interfaces
		"0.0.0.0:7777",      // IPv4 wildcard
		"[::]:7777",         // IPv6 wildcard
		"192.168.1.10:7777", // specific non-loopback IP
		"example.com:7777",  // hostname (can't prove loopback)
	}
	for _, addr := range addrs {
		_, err := NewServer(Options{Registry: reg, Addr: addr})
		if err == nil {
			t.Errorf("NewServer(Addr=%q, no auth): want refusal, got nil error", addr)
			continue
		}
		if !strings.Contains(err.Error(), "refusing to bind non-loopback") {
			t.Errorf("NewServer(Addr=%q) error %q: want the non-loopback refusal", addr, err)
		}
		// The error must tell the operator how to fix it.
		if !strings.Contains(err.Error(), "--attach-token") || !strings.Contains(err.Error(), DefaultListenAddr) {
			t.Errorf("NewServer(Addr=%q) error %q: want remediation hints (--attach-token, %s)", addr, err, DefaultListenAddr)
		}
	}
}

// TestNewServer_NonLoopbackAllowedWithAuth verifies each credential
// gate individually unlocks a non-loopback bind: bearer token, and
// enforced multi-session authentication. (mTLS also counts — covered
// by the listenerAuthenticated unit below, since it needs cert files.)
func TestNewServer_NonLoopbackAllowedWithAuth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts Options
	}{
		{"bearer token", Options{
			Registry: NewSessionRegistry(),
			Addr:     "0.0.0.0:0",
			Auth:     AuthConfig{BearerToken: "secret"},
		}},
		{"enforced multi-session", Options{
			Registry:            NewSessionRegistry(),
			Addr:                "0.0.0.0:0",
			MultiSessionEnabled: true,
		}},
	}
	for _, c := range cases {
		srv, err := NewServer(c.opts)
		if err != nil {
			t.Errorf("%s: NewServer: %v", c.name, err)
			continue
		}
		if err := srv.Bind(); err != nil {
			t.Errorf("%s: Bind: %v", c.name, err)
		}
		_ = srv.Close()
	}
}

// TestNewServer_MultiSessionAllowAnonymousStillRefused: multi-session
// with AllowAnonymous=true downgrades unauthenticated requests to the
// fallback caller — that is NOT a credential gate, so the non-loopback
// refusal still applies.
func TestNewServer_MultiSessionAllowAnonymousStillRefused(t *testing.T) {
	t.Parallel()
	_, err := NewServer(Options{
		Registry:            NewSessionRegistry(),
		Addr:                ":7777",
		MultiSessionEnabled: true,
		AllowAnonymous:      true,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to bind non-loopback") {
		t.Fatalf("multi-session + AllowAnonymous on :7777: want refusal, got %v", err)
	}
}

// TestNewServer_DefaultAddrIsLoopback verifies the zero-Addr default:
// leaving both Addr and UnixSocket empty binds DefaultListenAddr
// (127.0.0.1:7777) instead of erroring or binding all interfaces.
func TestNewServer_DefaultAddrIsLoopback(t *testing.T) {
	t.Parallel()
	srv, err := NewServer(Options{Registry: NewSessionRegistry()})
	if err != nil {
		t.Fatalf("NewServer with empty Addr/UnixSocket: %v", err)
	}
	defer func() { _ = srv.Close() }()
	if srv.opts.Addr != DefaultListenAddr {
		t.Fatalf("default Addr = %q, want %q", srv.opts.Addr, DefaultListenAddr)
	}
	if !isLoopbackAddr(srv.opts.Addr) {
		t.Fatalf("default Addr %q is not loopback", srv.opts.Addr)
	}
}

// TestBind_TokenlessLoopbackWarns verifies the loud startup warning:
// tokenless on loopback is allowed (local-dev posture) but must tell
// the operator that any local process can drive the agent.
// Not parallel — swaps the global log output.
func TestBind_TokenlessLoopbackWarns(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	srv, err := NewServer(Options{Registry: NewSessionRegistry(), Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() { _ = srv.Close() }()
	if err := srv.Bind(); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "NO authentication") {
		t.Fatalf("tokenless loopback Bind: want loud warning in log, got %q", out)
	}
}

// TestBind_WithTokenDoesNotWarn: the warning is strictly for the
// unauthenticated posture.
// Not parallel — swaps the global log output.
func TestBind_WithTokenDoesNotWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	srv, err := NewServer(Options{
		Registry: NewSessionRegistry(),
		Addr:     "127.0.0.1:0",
		Auth:     AuthConfig{BearerToken: "secret"},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() { _ = srv.Close() }()
	if err := srv.Bind(); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if strings.Contains(buf.String(), "NO authentication") {
		t.Fatalf("token-configured Bind should not warn; log = %q", buf.String())
	}
}

// TestNewServer_UnixSocketUnaffected: the bind policy is TCP-only —
// Unix sockets rely on filesystem permissions (0600) for auth.
func TestNewServer_UnixSocketUnaffected(t *testing.T) {
	t.Parallel()
	sock := t.TempDir() + "/attach.sock"
	srv, err := NewServer(Options{Registry: NewSessionRegistry(), UnixSocket: sock})
	if err != nil {
		t.Fatalf("NewServer(UnixSocket, no auth): %v", err)
	}
	_ = srv.Close()
}

func TestIsLoopbackAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:7777", true},
		{"127.0.0.2:7777", true}, // whole 127/8 block is loopback
		{"[::1]:7777", true},
		{"localhost:7777", true},
		{"LOCALHOST:7777", true},
		{":7777", false},
		{"0.0.0.0:7777", false},
		{"[::]:7777", false},
		{"10.1.2.3:7777", false},
		{"example.com:7777", false},
		{"garbage", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isLoopbackAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// TestListenerAuthenticated covers the mTLS arm (ClientCAFile) that
// the end-to-end tests skip (needs cert files on disk to Bind).
func TestListenerAuthenticated(t *testing.T) {
	t.Parallel()
	if listenerAuthenticated(Options{}) {
		t.Error("zero Options: want unauthenticated")
	}
	if !listenerAuthenticated(Options{Auth: AuthConfig{BearerToken: "t"}}) {
		t.Error("bearer token: want authenticated")
	}
	if !listenerAuthenticated(Options{Auth: AuthConfig{ClientCAFile: "/ca.pem"}}) {
		t.Error("mTLS client CA: want authenticated")
	}
	if !listenerAuthenticated(Options{MultiSessionEnabled: true}) {
		t.Error("enforced multi-session: want authenticated")
	}
	if listenerAuthenticated(Options{MultiSessionEnabled: true, AllowAnonymous: true}) {
		t.Error("multi-session + AllowAnonymous: want unauthenticated")
	}
}
