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
	"fmt"

	"github.com/go-steer/core-agent/pkg/auth"
	"github.com/go-steer/core-agent/pkg/config"
)

// buildMultiSessionAuthn translates the operator's
// attach.multi_session config block into the pkg/auth Authenticator
// that the attach listener consults per-request. Returns:
//
//   - authn: the resolved Authenticator (or nil for single-user mode)
//   - fallback: the Caller stamped on requests that don't authenticate
//     (used by callerMiddleware as the no-cred default)
//   - err: a fatal startup error if the config is internally
//     inconsistent OR a referenced file can't be loaded
//
// In single-user mode (multi_session.enabled = false), returns
// (nil, zero-Caller, nil) — the attach server defaults its own
// AnonymousAuth and the wiring is a no-op.
func buildMultiSessionAuthn(cfg config.MultiSessionConfig) (auth.Authenticator, auth.Caller, error) {
	// Default Caller comes from the config knob (resolved to "anon"
	// when unset to match the design doc's documented default). Used
	// for the legacy / single-user path AND as the AllowAnonymous
	// fallback when multi-session is on.
	defaultCaller := auth.Caller{Identity: cfg.DefaultIdentity}
	if defaultCaller.Identity == "" {
		defaultCaller = auth.Anonymous
	}

	if !cfg.Enabled {
		return nil, defaultCaller, nil
	}

	switch cfg.Auth.Kind {
	case "", config.MultiSessionAuthKindBearerTable:
		users, err := auth.LoadUsersFile(cfg.Auth.TableFile)
		if err != nil {
			return nil, defaultCaller, fmt.Errorf("load users file: %w", err)
		}
		authn := auth.NewBearerTokenAuth(users.Users, cfg.AdminIdentities, cfg.ProxyIdentities)
		return authn, defaultCaller, nil
	default:
		// Validation in config.Validate() should catch this earlier;
		// guard anyway so a corrupted call path produces a clear error
		// instead of a silent fallback.
		return nil, defaultCaller, fmt.Errorf("unsupported auth.kind %q (only %q is shipped in this version)", cfg.Auth.Kind, config.MultiSessionAuthKindBearerTable)
	}
}
