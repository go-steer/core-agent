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
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// Options configures NewServer. Zero value is invalid — Registry is
// required at minimum.
type Options struct {
	// Registry is the SessionRegistry the server consults to resolve
	// URL session IDs to live agents. Required.
	Registry *SessionRegistry

	// Auth controls TLS + bearer + read-only enforcement. Zero value
	// accepts everything (safe only over a Unix socket or other
	// already-trusted transport).
	Auth AuthConfig

	// Addr is the TCP listen address (e.g. ":7777"). Mutually
	// exclusive with UnixSocket — set exactly one.
	Addr string

	// UnixSocket is the Unix domain socket path (e.g.
	// "/var/run/core-agent.sock"). Mutually exclusive with Addr.
	// When set, Auth.TLS* and Auth.BearerToken are usually omitted
	// — filesystem permissions on the socket file are the auth.
	UnixSocket string

	// ShutdownTimeout caps how long Server.Close waits for in-flight
	// SSE clients to drain. Default 5 seconds.
	ShutdownTimeout time.Duration
}

// Server hosts the attach-mode HTTP endpoints. Construct via
// NewServer; start via ListenAndServe; stop via Close.
type Server struct {
	opts Options
	pool *BroadcasterPool
	mux  *http.ServeMux
	srv  *http.Server

	mu       sync.Mutex
	listener net.Listener
	closed   bool
}

// NewServer builds a Server. Validates Options; returns an error for
// invalid combinations (both/neither of Addr/UnixSocket; missing
// registry; TLS material mismatch).
func NewServer(opts Options) (*Server, error) {
	if opts.Registry == nil {
		return nil, errors.New("attach: Server: Options.Registry is required")
	}
	if opts.Addr == "" && opts.UnixSocket == "" {
		return nil, errors.New("attach: Server: exactly one of Options.Addr or Options.UnixSocket must be set")
	}
	if opts.Addr != "" && opts.UnixSocket != "" {
		return nil, errors.New("attach: Server: Options.Addr and Options.UnixSocket are mutually exclusive")
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 5 * time.Second
	}
	// Validate TLS material early so misconfig fails before we
	// touch the network.
	if _, err := opts.Auth.LoadTLSConfig(); err != nil {
		return nil, err
	}
	pool := NewBroadcasterPool()
	mux := http.NewServeMux()
	h := newHandlers(opts.Registry, pool)
	h.register(mux)

	return &Server{
		opts: opts,
		pool: pool,
		mux:  mux,
	}, nil
}

// ListenAndServe binds the listener and serves until Close or a fatal
// error. Blocks until the server stops. Returns nil on clean shutdown,
// the underlying error otherwise.
func (s *Server) ListenAndServe() error {
	ln, err := s.listen()
	if err != nil {
		return err
	}

	tlsCfg, err := s.opts.Auth.LoadTLSConfig()
	if err != nil {
		_ = ln.Close()
		return err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ln.Close()
		return errors.New("attach: Server: already closed")
	}
	s.listener = ln
	s.srv = &http.Server{
		Handler:           s.opts.Auth.Middleware(s.mux),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		// Streaming endpoints have no useful write timeout — SSE
		// connections live for minutes/hours. Don't set one.
	}
	s.mu.Unlock()

	if tlsCfg != nil {
		// ServeTLS with empty cert/key uses TLSConfig.Certificates
		// already populated by LoadTLSConfig.
		err = s.srv.ServeTLS(ln, "", "")
	} else {
		err = s.srv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// listen builds the net.Listener per the configured Addr / UnixSocket.
// Unix sockets are removed first if a stale file exists (typical
// after a crash).
func (s *Server) listen() (net.Listener, error) {
	if s.opts.UnixSocket != "" {
		// Best-effort remove of a stale socket file. If the listener
		// at the other end is alive, the subsequent Listen will fail
		// with "address in use", which is the right error.
		_ = os.Remove(s.opts.UnixSocket)
		ln, err := net.Listen("unix", s.opts.UnixSocket)
		if err != nil {
			return nil, fmt.Errorf("attach: listen unix %q: %w", s.opts.UnixSocket, err)
		}
		// Restrict the socket to the owner by default — local-dev
		// convenience implies "only this user". Operators wanting
		// group access can chmod the socket post-listen.
		if err := os.Chmod(s.opts.UnixSocket, 0o600); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("attach: chmod unix socket: %w", err)
		}
		return ln, nil
	}
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("attach: listen tcp %q: %w", s.opts.Addr, err)
	}
	return ln, nil
}

// Close stops the server, waits up to Options.ShutdownTimeout for
// in-flight SSE clients to disconnect, then tears down the broadcaster
// pool. Idempotent.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	srv := s.srv
	s.mu.Unlock()

	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
	defer cancel()
	shutdownErr := srv.Shutdown(ctx)
	s.pool.Close()
	return shutdownErr
}

// Addr returns the actual listener address the server bound to. Useful
// for tests that use Addr=":0" to get an ephemeral port. Returns empty
// before ListenAndServe runs.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// TLSEnabled reports whether the server is running with TLS (HTTPS
// endpoints) or plaintext (HTTP). Helpful for log lines / debugging.
func (s *Server) TLSEnabled() bool {
	_, _ = s.opts.Auth.LoadTLSConfig() // validate already-passed config
	return s.opts.Auth.TLSCertFile != ""
}

// peekTLSConfig is a test helper — exported with a lowercase name so
// only same-package tests can use it.
func (s *Server) peekTLSConfig() *tls.Config { //nolint:unused
	cfg, _ := s.opts.Auth.LoadTLSConfig()
	return cfg
}
