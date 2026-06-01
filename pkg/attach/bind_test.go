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
	"net"
	"strings"
	"testing"
)

// TestBindSurfacesPortInUse is the regression test for the v2.1 smoke
// bug: a second daemon started with --attach-listen=:N (where :N is
// already bound) silently fell through to REPL because the bind error
// was swallowed inside the listener goroutine. Bind in the main path
// surfaces the error synchronously so cmd/core-agent can exit non-zero.
func TestBindSurfacesPortInUse(t *testing.T) {
	t.Parallel()

	// Hold a TCP port to simulate the "first daemon already bound".
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer func() { _ = held.Close() }()
	addr := held.Addr().String()

	srv, err := NewServer(Options{Registry: NewSessionRegistry(), Addr: addr})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	err = srv.Bind()
	if err == nil {
		t.Fatalf("Bind on already-held port: want error, got nil")
	}
	if !strings.Contains(err.Error(), "address already in use") &&
		!strings.Contains(err.Error(), "bind:") {
		t.Fatalf("Bind error %q: want a bind/address-in-use error", err)
	}
}

// TestServeBeforeBind verifies the misuse case fails cleanly rather
// than panicking on nil dereference.
func TestServeBeforeBind(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(Options{Registry: NewSessionRegistry(), Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	err = srv.Serve()
	if err == nil {
		t.Fatalf("Serve before Bind: want error, got nil")
	}
	if !strings.Contains(err.Error(), "Serve called before Bind") {
		t.Fatalf("Serve error %q: want \"Serve called before Bind\"", err)
	}
}

// TestBindTwice verifies double-bind is rejected.
func TestBindTwice(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(Options{Registry: NewSessionRegistry(), Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	if err := srv.Bind(); err != nil {
		t.Fatalf("first Bind: %v", err)
	}
	err = srv.Bind()
	if err == nil {
		t.Fatalf("second Bind: want error, got nil")
	}
	if !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("Bind error %q: want \"already bound\"", err)
	}
}
