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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// DirectCaller names only an identity the server verified from the
// request's own credential: an operator's token, yes; a relay asserting
// that same operator, an anonymous request, or a context no middleware
// touched, no (#1175 decision 6).
func TestDirectCaller(t *testing.T) {
	t.Parallel()
	run := func(cfg callerMiddlewareConfig, headers map[string]string) string {
		var got string
		h := callerMiddlewareWithConfig(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = DirectCaller(r.Context())
		}))
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d", rr.Code)
		}
		return got
	}
	bearer := callerMiddlewareConfig{authenticator: newBearerAuthForProxy(t)}

	if got := run(bearer, map[string]string{"Authorization": "Bearer tok_alice"}); got != "alice@example.com" {
		t.Errorf("own token: DirectCaller = %q, want alice@example.com", got)
	}
	if got := run(bearer, map[string]string{"Authorization": "Bearer tok_bot", auth.HeaderAssertedCaller: "alice@example.com"}); got != "" {
		t.Errorf("relay asserting alice: DirectCaller = %q, want \"\"", got)
	}
	if got := run(callerMiddlewareConfig{}, nil); got != "" {
		t.Errorf("anonymous: DirectCaller = %q, want \"\"", got)
	}
	// A transport token verifies the request, not whose it is: every
	// holder of the token — the operator's TUI and a watcher alike —
	// resolves to the same configured default identity.
	shared := callerMiddlewareConfig{transportBearerConfigured: true, fallback: auth.Caller{Identity: "operator"}}
	if got := run(shared, nil); got != "" {
		t.Errorf("transport bearer, anonymous authenticator: DirectCaller = %q, want \"\" — the identity is the shared default", got)
	}
	// Nor does a verdict stamped outside the middleware count.
	stamped := withAuthSource(auth.WithCaller(context.Background(), auth.Caller{Identity: "alice@example.com"}), whoAmISourceBearer)
	if got := DirectCaller(stamped); got != "" {
		t.Errorf("source stamped without the middleware: DirectCaller = %q, want \"\"", got)
	}
	if got := DirectCaller(auth.WithCaller(context.Background(), auth.Caller{Identity: "alice@example.com"})); got != "" {
		t.Errorf("no middleware ran: DirectCaller = %q, want \"\"", got)
	}
}
