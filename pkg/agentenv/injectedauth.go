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

package agentenv

import (
	"context"
	"os"
	"time"

	"golang.org/x/oauth2"
)

// EnvGoogleAuth names the environment variable that switches Google
// credential resolution into injected-auth mode.
//
// When CORE_AGENT_GOOGLE_AUTH=injected, Google clients (Vertex AI models,
// Anthropic on Vertex AI, Vertex MaaS profiles, and Google-managed MCP
// servers) skip Application Default Credentials detection and send
// "Authorization: Bearer placeholder" on every request. An egress proxy
// outside the sandbox (such as Agent Substrate's egress gateway) replaces
// the header with a real Google OAuth access token before forwarding, so
// the sandbox and its snapshots never hold a credential.
const EnvGoogleAuth = "CORE_AGENT_GOOGLE_AUTH"

// GoogleAuthInjected is the value of EnvGoogleAuth that enables
// placeholder bearer authentication.
const GoogleAuthInjected = "injected"

// InjectedGooglePlaceholder is the bearer value sent when
// CORE_AGENT_GOOGLE_AUTH=injected. The egress gateway overwrites the
// whole Authorization header; this only satisfies client libraries that
// refuse to send an unauthenticated request.
const InjectedGooglePlaceholder = "placeholder"

// InjectedGoogleAuth reports whether CORE_AGENT_GOOGLE_AUTH=injected is
// set in the current process environment.
func InjectedGoogleAuth() bool {
	return InjectedGoogleAuthFrom(os.Getenv)
}

// InjectedGoogleAuthFrom reports whether getenv(EnvGoogleAuth) == "injected".
// A nil getenv defaults to os.Getenv.
func InjectedGoogleAuthFrom(getenv func(string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	return getenv(EnvGoogleAuth) == GoogleAuthInjected
}

// InjectedGoogleTokenSource returns an oauth2.TokenSource that always
// yields InjectedGooglePlaceholder without network or ADC access.
func InjectedGoogleTokenSource() oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: InjectedGooglePlaceholder,
		TokenType:   "Bearer",
		Expiry:      time.Now().Add(100 * 365 * 24 * time.Hour),
	})
}

// InjectedGoogleStatic satisfies github.com/go-steer/core-models/auth.TokenSource
// with InjectedGooglePlaceholder, without importing core-models into agentenv.
type InjectedGoogleStatic struct{}

// Token returns InjectedGooglePlaceholder.
func (InjectedGoogleStatic) Token(context.Context) (string, error) {
	return InjectedGooglePlaceholder, nil
}
