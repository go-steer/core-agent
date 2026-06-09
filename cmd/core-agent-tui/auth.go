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
// Two strategies ship today:
//
//   - "bearer" (default): existing direct-attach path —
//     Authorization: Bearer <attach-token>. Works for local, Unix-
//     socket, K8s internal LB, anywhere the operator owns the
//     Authorization header.
//
//   - "google-id-token": Cloud Run IAM path — mint a Google ID token
//     via Application Default Credentials, send it as
//     Authorization: Bearer <ID-token> (the gateway validates), and
//     send the attach token as X-Attach-Token (core-agent validates).
//     Audience is derived from the connection URL — for Cloud Run,
//     the service URL is the right audience.
//
// Future strategies (mTLS, header-cmd escape hatch, IAP with explicit
// audience override, Cloudflare Access JWT, …) plug in here.

package main

import (
	"context"
	"fmt"

	"google.golang.org/api/idtoken"

	"github.com/go-steer/core-agent/internal/attachclient"
)

// Auth mode names. Keep in sync with the --auth flag's usage string.
const (
	authModeBearer        = "bearer"
	authModeGoogleIDToken = "google-id-token"
)

// resolveCredentials maps --auth's string value to the right
// attachclient.Credentials value. attachToken is the value resolved
// from --token-env; may be empty in IAM-only postures where the
// daemon runs without --attach-token.
func resolveCredentials(ctx context.Context, mode string, parsed *attachclient.ParsedURL, attachToken string) (attachclient.Credentials, error) {
	switch mode {
	case "", authModeBearer:
		return attachclient.BearerCreds{Token: attachToken}, nil
	case authModeGoogleIDToken:
		audience, err := audienceFromURL(parsed)
		if err != nil {
			return nil, fmt.Errorf("--auth=%s: %w", authModeGoogleIDToken, err)
		}
		src, err := idtoken.NewTokenSource(ctx, audience)
		if err != nil {
			return nil, fmt.Errorf("--auth=%s: Application Default Credentials unavailable (run 'gcloud auth application-default login' or set GOOGLE_APPLICATION_CREDENTIALS): %w", authModeGoogleIDToken, err)
		}
		return attachclient.GoogleIDTokenCreds{
			Source:      src,
			AttachToken: attachToken,
		}, nil
	default:
		return nil, fmt.Errorf("--auth=%q: unknown strategy; want one of %q, %q", mode, authModeBearer, authModeGoogleIDToken)
	}
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
