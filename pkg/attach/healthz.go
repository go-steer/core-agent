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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// HealthzPath is the unauthenticated readiness endpoint (#946). It is
// served ahead of the auth middleware, so it is reachable without a
// bearer token and cannot be reached with one either — the middleware
// never sees it. Exported so recipes and tests refer to one constant
// rather than a repeated string literal.
const HealthzPath = "/healthz"

// healthCheckTimeout bounds every check on a single probe. A wedged
// dependency must make the probe report "not ready", not hang: kubelet
// gives up on its own timeoutSeconds but the handler goroutine would
// stay parked, and at one probe every periodSeconds that leaks
// goroutines against exactly the dependency that is already sick.
const healthCheckTimeout = 2 * time.Second

// newHealthLogger returns a report sink that writes one line per
// health *transition* rather than one line per probe.
//
// A readiness probe runs every few seconds forever. A sink that logged
// each failure would turn a single sick dependency into thousands of
// identical lines, which is how the one line that mattered gets lost.
// So it needs every outcome, not just the failures: without the
// successes it cannot tell a recovery from a gap, and a check that
// broke, healed and broke again would log once and then stay quiet
// through the second outage.
//
// The first probe of a healthy check logs nothing — a daemon that
// starts and stays fine should be silent.
func newHealthLogger(w io.Writer) func(name string, healthy bool, err error) {
	var mu sync.Mutex
	// nil entry = never observed. Otherwise the last reported state.
	seen := map[string]bool{}
	return func(name string, healthy bool, err error) {
		mu.Lock()
		prev, known := seen[name]
		seen[name] = healthy
		mu.Unlock()
		switch {
		case !known && healthy:
			// Silent: the ordinary case.
		case healthy && !prev:
			fmt.Fprintf(w, "core-agent: healthz: %s recovered\n", name)
		case !healthy && (!known || prev):
			fmt.Fprintf(w, "core-agent: healthz: %s is not ready: %v\n", name, err)
		}
	}
}

// HealthCheck is one named subsystem probe run on GET /healthz.
//
// Name appears verbatim in the response body, so it must be a
// compile-time constant chosen by the daemon and never derived from
// request data, configuration, or anything else an operator could use
// it to exfiltrate — the endpoint is unauthenticated.
//
// Check must be read-only, must not make outbound calls (the endpoint
// reports subsystem readiness, not model-provider liveness), and must
// honour ctx.
type HealthCheck struct {
	Name  string
	Check func(context.Context) error
}

// health status words. The body carries these and nothing else: an
// error string from a failing check can contain a filesystem path, a
// DSN, or a hostname, none of which an unauthenticated caller is
// entitled to. The detail goes to the daemon's logs, where the
// operator who can read them is already authenticated to the node.
const (
	healthReady   = "ready"
	healthFailed  = "failed"
	healthTimeout = "timeout"
)

// healthzResponse is the entire public surface of the endpoint.
//
// Deliberately absent: session counts, session IDs, caller identities,
// the daemon's version, the configured auth mode. Each would be a
// small gift to an unauthenticated scanner, and none of them is what a
// readiness probe is asking about.
type healthzResponse struct {
	OK     bool              `json:"ok"`
	Checks map[string]string `json:"checks,omitempty"`
}

// healthzHandler serves GET /healthz.
//
// 200 when every check passes, 503 when any fails — kubelet reads the
// status code, so the code is the part that must be right. The body is
// for the human reading `curl` output during an incident.
//
// With no checks registered the endpoint reports {"ok":true} and
// nothing else. That is honest: it then proves exactly what a
// tcpSocket probe proves, which is that this process is accepting and
// routing HTTP, and it claims no more.
// report, when non-nil, is called once per check per probe with the
// check's outcome. It exists so the daemon can log the error text the
// body deliberately withholds; see newHealthLogger for why it is given
// every outcome rather than only the failures.
func healthzHandler(checks []HealthCheck, report func(name string, healthy bool, err error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The route bypasses the mux (see publicBypass), so method
		// matching is this handler's job rather than the router's.
		// HEAD is allowed because probes and load balancers use it and
		// net/http elides the body for us.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		resp := healthzResponse{OK: true}
		if len(checks) > 0 {
			resp.Checks = make(map[string]string, len(checks))
		}
		for _, c := range checks {
			if c.Check == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
			err := c.Check(ctx)
			timedOut := ctx.Err() != nil
			cancel()
			switch {
			case err == nil:
				resp.Checks[c.Name] = healthReady
			case timedOut:
				resp.Checks[c.Name] = healthTimeout
				resp.OK = false
			default:
				resp.Checks[c.Name] = healthFailed
				resp.OK = false
			}
			if report != nil {
				report(c.Name, err == nil, err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		// Probes are polled; a cached 200 outlives the outage it is
		// supposed to report.
		w.Header().Set("Cache-Control", "no-store")
		if !resp.OK {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
}
