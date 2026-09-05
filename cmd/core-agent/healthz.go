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
	"context"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// daemonHealthChecks builds the subsystem probes behind the attach
// listener's unauthenticated GET /healthz (#946).
//
// The endpoint's whole justification is being strictly more
// informative than the tcpSocket probe recipes fall back to today, so
// every entry here has to be something that can actually be false
// while the listener is still accepting connections. That rules out
// most of what a health endpoint is tempted to report:
//
//   - "auth": loaded — the users file is read once, at boot, and a
//     failure there exits before the listener binds. A field for it
//     could only ever say "loaded". Reporting it would put a green
//     check that measures nothing on the endpoint that exists because
//     coarse readiness signals cost us an afternoon.
//   - the model provider — an outbound call would make kubelet's
//     readiness gate depend on a third party's availability, and a
//     provider outage would roll the pod rather than report the
//     outage.
//   - session counts, identities, version — the caller is
//     unauthenticated.
//
// What is left is the session database, which is genuinely dynamic:
// the file can be deleted out from under a running daemon, the volume
// can go read-only, another writer can hold the lock. Every attach
// shape has one since #973 made attach mode imply it, so in daemon
// mode this always returns exactly one check.
//
// A nil handle yields no checks rather than a passing one. That
// degrades /healthz to what TCP already proves, which is the honest
// answer when there is nothing to interrogate.
func daemonHealthChecks(h *eventlog.Handle) []attach.HealthCheck {
	if h == nil {
		return nil
	}
	return []attach.HealthCheck{{
		Name:  "session_db",
		Check: func(ctx context.Context) error { return h.Ping(ctx) },
	}}
}
