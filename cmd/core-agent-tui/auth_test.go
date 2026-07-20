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
	"errors"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
)

func TestAudienceFromURL(t *testing.T) {
	cases := []struct {
		name    string
		rawURL  string
		want    string
		wantErr bool
	}{
		{
			name:   "https Cloud Run service URL → audience is scheme+host",
			rawURL: "https://my-svc-abc123-uc.a.run.app",
			want:   "https://my-svc-abc123-uc.a.run.app",
		},
		{
			name:   "https with port preserved",
			rawURL: "https://attach.example.com:8443",
			want:   "https://attach.example.com:8443",
		},
		{
			name:   "https with path stripped (audience matches on prefix)",
			rawURL: "https://my-svc.run.app/sessions/abc",
			want:   "https://my-svc.run.app",
		},
		{
			name:    "http rejected — meaningless for gateway-fronted auth",
			rawURL:  "http://localhost:7777",
			wantErr: true,
		},
		{
			name:    "unix:// rejected — direct-attach transport",
			rawURL:  "unix:///tmp/core-agent.sock",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := attachclient.ParseURL(tc.rawURL)
			if err != nil {
				t.Fatalf("ParseURL: %v", err)
			}
			got, err := audienceFromURL(parsed)
			if tc.wantErr {
				if err == nil {
					t.Errorf("audienceFromURL(%q) = %q, want error", tc.rawURL, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("audienceFromURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("audienceFromURL(%q) = %q, want %q", tc.rawURL, got, tc.want)
			}
		})
	}
}

func TestAudienceFromURL_NilSafe(t *testing.T) {
	if _, err := audienceFromURL(nil); err == nil {
		t.Errorf("audienceFromURL(nil): want error, got nil")
	}
}

func TestResolveCredentials_BearerDefault(t *testing.T) {
	// Default --auth is "bearer" — same behavior as the empty
	// string for back-compat with operators who don't set the flag.
	parsed, _ := attachclient.ParseURL("http://localhost:7777")
	for _, mode := range []string{"", "bearer"} {
		t.Run("mode="+mode, func(t *testing.T) {
			creds, err := resolveCredentials(t.Context(), mode, parsed, "attach-secret")
			if err != nil {
				t.Fatalf("resolveCredentials: %v", err)
			}
			bc, ok := creds.(attachclient.BearerCreds)
			if !ok {
				t.Fatalf("creds = %T, want attachclient.BearerCreds", creds)
			}
			if bc.Token != "attach-secret" {
				t.Errorf("BearerCreds.Token = %q, want %q", bc.Token, "attach-secret")
			}
		})
	}
}

func TestResolveCredentials_UnknownMode(t *testing.T) {
	parsed, _ := attachclient.ParseURL("http://localhost:7777")
	_, err := resolveCredentials(t.Context(), "magic", parsed, "x")
	if err == nil {
		t.Fatalf("resolveCredentials(magic): want error, got nil")
	}
	if !strings.Contains(err.Error(), "magic") {
		t.Errorf("error %q should mention the bad mode value", err.Error())
	}
}

func TestResolveCredentials_GoogleIDToken_HTTPRejected(t *testing.T) {
	// google-id-token over http:// is meaningless (gateway-fronted
	// auth requires https). resolveCredentials should surface this
	// as a clear error before attempting any ADC resolution.
	parsed, _ := attachclient.ParseURL("http://localhost:7777")
	_, err := resolveCredentials(t.Context(), "google-id-token", parsed, "x")
	if err == nil {
		t.Fatalf("resolveCredentials(google-id-token, http://): want error, got nil")
	}
	// The error path should mention http (so operators see the
	// constraint), not pretend ADC failed.
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("error %q should reference the http:// constraint", err.Error())
	}
}

func TestResolveCredentials_GoogleOAuth_HTTPRejected(t *testing.T) {
	// Same constraint as google-id-token — gateway-fronted auth
	// requires https. Verified separately because the error path
	// goes through a different branch (requireHTTPSForGateway vs
	// audienceFromURL).
	parsed, _ := attachclient.ParseURL("http://localhost:7777")
	_, err := resolveCredentials(t.Context(), "google-oauth", parsed, "x")
	if err == nil {
		t.Fatalf("resolveCredentials(google-oauth, http://): want error, got nil")
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("error %q should reference the http:// constraint", err.Error())
	}
}

func TestResolveCredentials_GoogleOAuth_UnixRejected(t *testing.T) {
	parsed, _ := attachclient.ParseURL("unix:///tmp/sock")
	_, err := resolveCredentials(t.Context(), "google-oauth", parsed, "x")
	if err == nil {
		t.Fatalf("resolveCredentials(google-oauth, unix://): want error, got nil")
	}
	if !strings.Contains(err.Error(), "unix") || !strings.Contains(err.Error(), "bearer") {
		t.Errorf("error %q should mention unix transport + suggest --auth=bearer", err.Error())
	}
}

func TestExplainIDTokenSourceError_AuthorizedUser(t *testing.T) {
	// The cryptic "unsupported credentials type: authorized_user"
	// error from idtoken.NewTokenSource is the #1 operator footgun
	// (every end-user-ADC operator hits it on first try). The
	// rewritten error must surface BOTH workarounds so operators
	// don't have to grep docs.
	raw := errors.New(`idtoken: unsupported credentials type: "authorized_user"`)
	rewritten := explainIDTokenSourceError(raw)
	msg := rewritten.Error()
	for _, want := range []string{
		"--auth=google-oauth",         // primary recommendation
		"impersonate-service-account", // alternative workaround
		"serviceAccountTokenCreator",  // permission operator needs
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("rewritten error should contain %q; got:\n%s", want, msg)
		}
	}
	// The original error must still be wrappable via errors.Is so
	// callers (and stack traces) preserve provenance.
	if !errors.Is(rewritten, raw) {
		t.Errorf("rewritten error should wrap the original")
	}
}

func TestExplainIDTokenSourceError_OtherErrorPassesThrough(t *testing.T) {
	// Unrelated failures (network, missing ADC, etc.) shouldn't
	// claim authorized-user is the cause. They get the generic
	// ADC-unavailable hint instead.
	raw := errors.New("metadata server unreachable")
	rewritten := explainIDTokenSourceError(raw)
	msg := rewritten.Error()
	if strings.Contains(msg, "authorized_user") || strings.Contains(msg, "impersonate") {
		t.Errorf("unrelated error should NOT hijack the authorized_user message:\n%s", msg)
	}
	if !strings.Contains(msg, "gcloud auth application-default login") {
		t.Errorf("non-authorized-user error should still point at ADC setup; got:\n%s", msg)
	}
	if !errors.Is(rewritten, raw) {
		t.Errorf("rewritten error should wrap the original")
	}
}

// NOTE: testing the google-id-token happy path requires hitting the
// real ADC stack (idtoken.NewTokenSource). That isn't appropriate
// for a unit test (no creds in CI). The Credentials interface itself
// is tested over a static token source in
// internal/attachclient/credentials_test.go — that's where the
// integration-shape coverage lives. Here we cover the resolver's
// surface (flag parsing, URL validation, error paths) only.
