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

// Resolver between the operator-visible --auth flag and the
// attachclient.Credentials values the HTTP client consumes.
//
// Three strategies ship today:
//
//   - "bearer" (default): existing direct-attach path —
//     Authorization: Bearer <attach-token>. Works for local, Unix-
//     socket, K8s internal LB, anywhere the operator owns the
//     Authorization header.
//
//   - "google-oauth": Cloud Run IAM (recommended). Uses
//     google.FindDefaultCredentials to source an OAuth2 access
//     token from ADC, stamps Authorization: Bearer <access-token>.
//     Works with every ADC shape operators actually have on their
//     workstations: end-user creds from `gcloud auth application-
//     default login`, service-account JSON keys, metadata server,
//     impersonation. Mirrors MCP's googleAuthTransport pattern
//     (pkg/mcp/lifecycle.go).
//
//   - "google-id-token": audience-bound variant. Mints a Google ID
//     token via idtoken.NewTokenSource — required by IAP (audience
//     = OAuth client ID), optional for Cloud Run IAM. Does NOT
//     work with end-user ADC (idtoken only accepts service-account-
//     shaped credentials). Operators using end-user ADC must either
//     re-login with impersonation
//     (`gcloud auth application-default login
//     --impersonate-service-account=SA`) or set
//     GOOGLE_APPLICATION_CREDENTIALS to a service-account JSON key.
//
// Future strategies (mTLS, header-cmd escape hatch, IAP with
// explicit audience override, Cloudflare Access JWT, …) plug in here.

package main

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/idtoken"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
)

// Auth mode names. Keep in sync with the --auth flag's usage string.
const (
	authModeBearer        = "bearer"
	authModeGoogleOAuth   = "google-oauth"
	authModeGoogleIDToken = "google-id-token"
)

// googleOAuthScope is what we ask for when sourcing OAuth tokens via
// ADC. Cloud Run IAM doesn't care about scope (the IAM check is on
// the user's identity, not the token's scope), and end-user ADC's
// default login already grants this scope — so requesting it succeeds
// without forcing operators to re-login. Mirrors what most Google
// CLI tooling defaults to.
const googleOAuthScope = "https://www.googleapis.com/auth/cloud-platform"

// resolveCredentials maps --auth's string value to the right
// attachclient.Credentials value. attachToken is the value resolved
// from --token-env; may be empty in IAM-only postures where the
// daemon runs without --attach-token.
func resolveCredentials(ctx context.Context, mode string, parsed *attachclient.ParsedURL, attachToken string) (attachclient.Credentials, error) {
	switch mode {
	case "", authModeBearer:
		return attachclient.BearerCreds{Token: attachToken}, nil
	case authModeGoogleOAuth:
		if err := requireHTTPSForGateway(parsed); err != nil {
			return nil, fmt.Errorf("--auth=%s: %w", authModeGoogleOAuth, err)
		}
		creds, err := google.FindDefaultCredentials(ctx, googleOAuthScope)
		if err != nil {
			return nil, fmt.Errorf("--auth=%s: Application Default Credentials unavailable (run 'gcloud auth application-default login' or set GOOGLE_APPLICATION_CREDENTIALS): %w", authModeGoogleOAuth, err)
		}
		// Fail-fast: pre-fetch a token so misconfigured ADC surfaces
		// at startup rather than on the first attach request.
		if _, err := creds.TokenSource.Token(); err != nil {
			return nil, fmt.Errorf("--auth=%s: initial token fetch: %w", authModeGoogleOAuth, err)
		}
		return attachclient.GoogleOAuthCreds{
			Source:      creds.TokenSource,
			AttachToken: attachToken,
		}, nil
	case authModeGoogleIDToken:
		audience, err := audienceFromURL(parsed)
		if err != nil {
			return nil, fmt.Errorf("--auth=%s: %w", authModeGoogleIDToken, err)
		}
		src, err := idtoken.NewTokenSource(ctx, audience)
		if err != nil {
			return nil, fmt.Errorf("--auth=%s: %w", authModeGoogleIDToken, explainIDTokenSourceError(err))
		}
		return attachclient.GoogleIDTokenCreds{
			Source:      src,
			AttachToken: attachToken,
		}, nil
	default:
		return nil, fmt.Errorf("--auth=%q: unknown strategy; want one of %q, %q, %q", mode, authModeBearer, authModeGoogleOAuth, authModeGoogleIDToken)
	}
}

// requireHTTPSForGateway rejects non-https URLs early with an
// operator-friendly message. Gateway-fronted auth is meaningless
// over plain http (the gateway terminates TLS and validates the
// token; sending a Bearer over http defeats both halves) and
// over unix:// (direct-attach transport — use --auth=bearer).
func requireHTTPSForGateway(parsed *attachclient.ParsedURL) error {
	if parsed == nil {
		return fmt.Errorf("nil URL")
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		return fmt.Errorf("http:// transport not supported (gateway-fronted auth requires https)")
	case "unix":
		return fmt.Errorf("unix:// transport is direct-attach (no gateway); use --auth=bearer instead")
	default:
		return fmt.Errorf("unrecognized URL scheme %q", parsed.Scheme)
	}
}

// explainIDTokenSourceError detects the most common failure mode of
// idtoken.NewTokenSource — end-user (authorized_user) ADC credentials,
// which it doesn't accept — and rewrites the cryptic underlying error
// into a clear operator-facing message with the two workarounds.
// Other errors pass through with their original wording.
func explainIDTokenSourceError(err error) error {
	msg := err.Error()
	if !strings.Contains(msg, "authorized_user") {
		// Unrelated failure — fall back to the same hint we use for
		// --auth=google-oauth so operators at least know where ADC
		// configuration lives.
		return fmt.Errorf("failed to load Application Default Credentials (run 'gcloud auth application-default login' or set GOOGLE_APPLICATION_CREDENTIALS): %w", err)
	}
	return fmt.Errorf(
		"idtoken does not accept end-user ADC (authorized_user). For Cloud Run IAM, "+
			"prefer --auth=%s which uses OAuth access tokens and works with end-user ADC. "+
			"To stay on --auth=%s (required for IAP audience-bound tokens), re-login ADC with "+
			"service-account impersonation:\n"+
			"  gcloud auth application-default login --impersonate-service-account=SA_EMAIL\n"+
			"(your user needs roles/iam.serviceAccountTokenCreator on SA_EMAIL.) "+
			"Underlying error: %w",
		authModeGoogleOAuth, authModeGoogleIDToken, err,
	)
}

// audienceFromURL derives a Google ID token audience from the
// connection URL. For Cloud Run, the service URL itself is the
// correct audience (e.g.
// https://my-svc-abc123-uc.a.run.app → audience is that URL exactly).
//
// Constraints:
//
//   - Scheme must be https. ID tokens are useless over plain http
//     (the gateway terminates TLS and validates the token; sending
//     a Bearer ID token over http defeats both halves).
//   - Unix socket and bare http URLs are rejected — the gateway-
//     fronted auth mode is meaningless for those transports.
//
// IAP (where audience is the OAuth client ID, not the URL) would
// land via an explicit --auth-audience override; that flag is a
// future addition once we have a target to validate against.
func audienceFromURL(parsed *attachclient.ParsedURL) (string, error) {
	if parsed == nil {
		return "", fmt.Errorf("audience: nil URL")
	}
	switch parsed.Scheme {
	case "https":
		// Cloud Run IAM expects the service URL (scheme + host) as
		// the audience. Strip path/query — Google's ID token check
		// matches on the URL prefix, not the full path.
		return fmt.Sprintf("https://%s", parsed.Host), nil
	case "http":
		return "", fmt.Errorf("audience: http:// transport not supported for google-id-token auth (gateway-fronted deploys require https)")
	case "unix":
		return "", fmt.Errorf("audience: unix:// transport is direct-attach (no gateway); use --auth=bearer instead")
	default:
		return "", fmt.Errorf("audience: unrecognized URL scheme %q", parsed.Scheme)
	}
}
