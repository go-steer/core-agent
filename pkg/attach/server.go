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
	"io/fs"
	"net"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// Options configures NewServer. Zero value is invalid — Registry is
// required at minimum.
type Options struct {
	// Registry is the SessionRegistry the server consults to resolve
	// URL session IDs to live agents. Required.
	Registry *SessionRegistry

	// PeerRegistry, when non-nil, enables peer-registration endpoints
	// (POST /peers, GET /peers, etc.) — turning this listener into a
	// discovery hub for other agents. Nil means the peer endpoints
	// are not registered and POST /peers returns 404. See
	// docs/peer-registration-design.md.
	PeerRegistry *PeerRegistry

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

	// AgentCard, when its Description and ExternalURL are both
	// non-empty, enables the unauthenticated discovery endpoint
	// GET /.well-known/agent-card.json. Zero value disables the
	// endpoint (404). See docs/agent-card-design.md.
	AgentCard AgentCardConfig

	// Authenticator resolves the per-request Caller for the
	// multi-session attach layer. Nil defaults to auth.AnonymousAuth
	// with DefaultCaller as the identity — every request resolves to
	// the same anonymous Caller and downstream code sees a
	// Caller-on-context just like in a multi-session deployment.
	//
	// α.1 wiring is intentionally additive: the resolved Caller is
	// available via auth.CallerFromContext but no handler enforces
	// authorization yet. α.2 layers enforcement on top without
	// changing this field's shape. See
	// docs/multi-session-design.md and issue #162.
	Authenticator auth.Authenticator

	// DefaultCaller is the Caller stamped onto requests when
	// Authenticator is nil, or when the Authenticator returns
	// ErrUnauthenticated under the α.1 no-behavior-change posture.
	// Zero value resolves to auth.Anonymous.
	DefaultCaller auth.Caller

	// MultiSessionEnabled turns on per-session ACL enforcement in
	// session-scoped handlers (Authorize against entry.ACL), 401 on
	// unauthenticated requests when AllowAnonymous is false, and the
	// X-Asserted-Caller proxy-header resolution path. Default false
	// preserves single-user behavior end-to-end.
	//
	// Operators set this via config.AttachConfig.MultiSession.Enabled.
	MultiSessionEnabled bool

	// AllowAnonymous, when MultiSessionEnabled is true, lets requests
	// without a valid credential resolve to DefaultCaller rather than
	// returning 401. Dangerous in shared environments — every
	// unauthenticated request becomes the same Caller. Default false
	// (matches the config default).
	AllowAnonymous bool

	// ProxyHeader is the header name a proxy Caller uses to assert
	// the effective identity. Empty defaults to
	// auth.HeaderAssertedCaller ("X-Asserted-Caller"). Honored only
	// when MultiSessionEnabled is true.
	ProxyHeader string

	// UI, when non-nil, registers a /ui/* route that serves a SPA
	// bundle from the supplied filesystem (typically the
	// internal/webui embedded mast-web release, or a local checkout
	// via os.DirFS for development).
	//
	// The SPA loads same-origin against this listener so the attach
	// API and the UI share auth boundary and TLS cert — no separate
	// listener, no CORS allowlist required.
	//
	// Nil disables the route entirely (the default).
	UI fs.FS

	// SessionFactory, when non-nil, enables the POST /sessions
	// endpoint — operators can create owned sessions on demand
	// instead of being limited to the single startup-time session.
	// The factory is invoked once per POST /sessions request with
	// the request context and the authenticated Caller; it returns
	// a fresh Registrant that the handler then registers via
	// RegisterOwned(ag, caller.Identity) so the session's ACL stamps
	// the creator as Owner.
	//
	// Nil leaves POST /sessions returning 501 — the older
	// single-session deployment model where the daemon constructs
	// exactly one agent at startup.
	//
	// Per docs/multi-session-design.md §"Open questions" #1
	// (resolved 2026-06-12): explicit session-creation API is the
	// preferred pattern; the factory closure lives in cmd-level
	// wiring so the daemon can capture model/gate-template/tools/
	// MCP/eventlog config and synthesize fresh agents under the
	// same configuration.
	SessionFactory SessionFactory

	// Resumer, when non-nil, reconstructs sessions that exist on
	// disk but not in the in-memory registry. Wired by the daemon
	// from buildSessionResumer; nil leaves the legacy "miss = 404"
	// behavior in place (pre-v2.5 deployments). See
	// docs/session-resume-design.md.
	Resumer SessionResumer

	// SessionIdleTimeout, when > 0, enables the background sweep
	// that evicts in-memory Entries idle longer than this duration.
	// Evicted sessions remain resumable via the Resumer — eviction
	// is memory-only, not delete-from-disk. Zero (the default)
	// disables the sweep: sessions stay in memory until the daemon
	// stops. See docs/session-resume-design.md §"Lifecycle
	// primitive".
	SessionIdleTimeout time.Duration
}

// SessionFactory constructs a fresh Registrant for the POST /sessions
// endpoint. Implementations capture the daemon-wide config (model,
// tools, MCP toolsets, eventlog handle, instruction roots) in a
// closure and synthesize a new *agent.Agent (or any other Registrant)
// per call.
//
// caller is the authenticated identity that triggered the creation;
// the handler stamps this as the new session's ACL Owner via
// RegisterOwnedWithCancel, so the factory itself does NOT need to
// call Register / RegisterOwned. (That separation keeps the auth
// concerns in the handler and the construction concerns in the
// factory.)
//
// The returned CancelFunc (may be nil) is stored on the resulting
// Entry and invoked when the registry evicts the session (idle
// sweep or explicit Unregister). Typically it cancels the ctx
// driving the per-session wake loop so background work exits
// cleanly instead of leaking past the session's lifetime.
//
// ctx is the request context; honor cancellation if the factory does
// any IO (e.g., loading per-caller instructions).
type SessionFactory func(ctx context.Context, caller auth.Caller) (Registrant, context.CancelFunc, error)

// Server hosts the attach-mode HTTP endpoints. Construct via
// NewServer; start via ListenAndServe; stop via Close.
type Server struct {
	opts Options
	pool *BroadcasterPool
	mux  *http.ServeMux
	srv  *http.Server

	// cardHandler is the always-unauthenticated handler for
	// GET /.well-known/agent-card.json. nil when AgentCard is
	// disabled (the path then 404s through the regular mux).
	cardHandler http.Handler

	mu       sync.Mutex
	listener net.Listener
	closed   bool

	// sweepCancel stops the SessionIdleTimeout sweep goroutine
	// (started by Bind) when the server closes. Nil when
	// SessionIdleTimeout <= 0 (sweep not started).
	sweepCancel context.CancelFunc
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
	// Validate the card config early — half-populated configs are a
	// startup error, not a silent 404 at first registry fetch.
	if err := opts.AgentCard.Validate(); err != nil {
		return nil, err
	}
	pool := NewBroadcasterPool()
	mux := http.NewServeMux()
	h := newHandlers(opts.Registry, pool)
	h.enforceACL = opts.MultiSessionEnabled
	h.factory = opts.SessionFactory
	if opts.Resumer != nil {
		opts.Registry.WithResumer(opts.Resumer)
	}
	h.register(mux)
	if opts.PeerRegistry != nil {
		ph := newPeerHandlers(opts.PeerRegistry)
		ph.register(mux)
	}
	if opts.UI != nil {
		// Strip the /ui prefix so the embedded SPA's relative paths
		// resolve against the asset root rather than /ui/index.html.
		mux.Handle("/ui/", http.StripPrefix("/ui", uiHandler(opts.UI)))
		// Redirect /ui (no trailing slash) to /ui/ so the SPA's
		// relative asset URLs work from either landing path.
		mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
		})
	}
	var cardHandler http.Handler
	if opts.AgentCard.Enabled() {
		cardHandler = agentCardHandler(opts.AgentCard, opts.Registry, opts.Auth)
	}

	return &Server{
		opts:        opts,
		pool:        pool,
		mux:         mux,
		cardHandler: cardHandler,
	}, nil
}

// ListenAndServe binds the listener and serves until Close or a fatal
// error. Blocks until the server stops. Returns nil on clean shutdown,
// the underlying error otherwise.
//
// Equivalent to Bind followed by Serve. Callers that need to surface
// bind failures synchronously (e.g., to fail-fast on port-in-use
// instead of degrading) should call Bind directly in the main goroutine
// and Serve in a background goroutine.
func (s *Server) ListenAndServe() error {
	if err := s.Bind(); err != nil {
		return err
	}
	return s.Serve()
}

// Bind binds the listener and prepares TLS / http.Server state. Returns
// any bind error synchronously so callers can fail-fast (the original
// ListenAndServe path lost bind errors when run inside a background
// goroutine — operators who started a second daemon with the port
// already held silently fell through to REPL mode talking to the wrong
// process). Safe to call exactly once per Server.
func (s *Server) Bind() error {
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
	defer s.mu.Unlock()
	if s.closed {
		_ = ln.Close()
		return errors.New("attach: Server: already closed")
	}
	if s.listener != nil {
		_ = ln.Close()
		return errors.New("attach: Server: already bound")
	}
	s.listener = ln
	// Middleware order: transport-level auth (TLS handshake + bearer
	// token check in AuthConfig.Middleware) gates first, then the
	// per-caller Authenticator resolves identity onto the request
	// context for handlers to read via auth.CallerFromContext.
	mcfg := callerMiddlewareConfig{
		authenticator:         s.opts.Authenticator,
		fallback:              s.opts.DefaultCaller,
		enforceAuthentication: s.opts.MultiSessionEnabled && !s.opts.AllowAnonymous,
		proxyHeader:           s.opts.ProxyHeader,
	}
	handler := callerMiddlewareWithConfig(mcfg, s.mux)
	// Wrap the outermost handler with OTel HTTP instrumentation. This
	// extracts W3C traceparent from incoming requests (set by the
	// watcher's otelhttp-wrapped client per #217) and starts a server
	// span whose context propagates through the middleware chain +
	// into the handler goroutine — so downstream tool-call / MCP /
	// Vertex spans nest under the caller's trace. When telemetry is
	// off (no global tracer provider), the wrapper is a no-op.
	//
	// SpanNameFormatter uses the route's method+path pattern for
	// grep-friendly span names in the collector (e.g. "POST /inject").
	tracedHandler := otelhttp.NewHandler(
		cardBypass(s.cardHandler, s.opts.Auth.Middleware(handler)),
		"daemon.attach",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
		// Skip tracing on the polling read endpoints — the remote TUI
		// hits /status + /usage every ~1-2s for status-bar rendering,
		// producing 30+ spans/min of pure noise. Same for the other
		// hydration reads that back /tools /mcp /pricing /perms /memory
		// /skills /context /agents slash commands. Traces stay meaningful
		// for the write path (/inject, /wake, /interrupt, /slash/*) and
		// SSE stream (/events, /perms/stream) — anything that actually
		// exercises the agent loop. Kept as a Filter (not a sampler
		// ratio) so operator writes never accidentally drop out.
		otelhttp.WithFilter(shouldTraceRequest),
	)
	s.srv = &http.Server{
		Handler:           tracedHandler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		// Streaming endpoints have no useful write timeout — SSE
		// connections live for minutes/hours. Don't set one.
	}
	// Start the idle-session sweep once the listener is bound.
	// Zero timeout disables — matches the "0s = keep everything
	// in memory forever" config contract. Sweep exits when
	// sweepCancel fires (called from Close).
	if s.opts.SessionIdleTimeout > 0 {
		sweepCtx, cancel := context.WithCancel(context.Background())
		s.sweepCancel = cancel
		go s.opts.Registry.SweepIdle(sweepCtx, s.opts.SessionIdleTimeout)
	}
	return nil
}

// cardBypass routes the well-known agent-card path directly to the
// (unauthenticated) card handler, falling through to the auth-
// protected mux for everything else. The card is a public discovery
// document by A2A convention — auth on the card defeats discovery.
//
// When card is nil (AgentCard disabled), every request goes to the
// protected handler and /.well-known/agent-card.json returns 404
// from the mux as expected.
// pollingReadRe matches the read endpoints the remote TUI polls every
// 1-2s for status-bar rendering. Filter used by otelhttp.WithFilter to
// suppress span creation on these paths — otherwise Cloud Trace fills
// with per-poll spans (30+/min) that drown the actually interesting
// tool-call and inject traces. Deliberately narrow: only GETs, only
// the hydration reads. Write paths (/inject, /wake, /interrupt,
// /slash/*), SSE streams (/events, /perms/stream), and admin ops
// (DELETE /sessions) all continue to trace.
//
// Three alternations:
//   - `/sessions` bare — the session-picker enumeration hit on every
//     TUI startup + on every fleet-view refresh.
//   - `/peers` bare — analogous peer enumeration for the multi-daemon
//     picker; polled at the same cadence.
//   - `/sessions/{app?}/{sid}/{leaf}` — the per-session hydration reads
//     the status bar polls every 1-2s.
var pollingReadRe = regexp.MustCompile(
	`^/sessions$` +
		`|^/peers$` +
		`|^/sessions/(?:[^/]+/)?[^/]+/(?:status|usage|tools|agents|context|memory|skills|mcp|pricing|perms)$`,
)

func shouldTraceRequest(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return true
	}
	return !pollingReadRe.MatchString(r.URL.Path)
}

func cardBypass(card http.Handler, protected http.Handler) http.Handler {
	if card == nil {
		return protected
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-card.json" {
			card.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

// Serve serves on the already-bound listener. Bind must have been
// called first. Blocks until the server stops. Returns nil on clean
// shutdown, the underlying error otherwise.
func (s *Server) Serve() error {
	s.mu.Lock()
	ln := s.listener
	srv := s.srv
	s.mu.Unlock()

	if ln == nil || srv == nil {
		return errors.New("attach: Server: Serve called before Bind")
	}
	tlsCfg := srv.TLSConfig

	var err error
	if tlsCfg != nil {
		// ServeTLS with empty cert/key uses TLSConfig.Certificates
		// already populated by LoadTLSConfig.
		err = srv.ServeTLS(ln, "", "")
	} else {
		err = srv.Serve(ln)
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
	sweepCancel := s.sweepCancel
	s.mu.Unlock()

	// Stop the idle sweep before shutdown so it doesn't try to
	// evict during teardown.
	if sweepCancel != nil {
		sweepCancel()
	}
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
