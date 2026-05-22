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

package tools

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/permissions"
)

// fetchGate returns a yolo-mode gate (URL pattern matching is tested
// separately via the urlMatcher unit tests).
func fetchGate(t *testing.T) *permissions.Gate {
	t.Helper()
	return permissions.New(permissions.Options{Mode: permissions.ModeYolo})
}

func fetchCfg(allow, deny []string) *config.Config {
	c := config.DefaultConfig()
	c.URLScope = config.URLScopeConfig{Allow: allow, Deny: deny}
	return c
}

func TestFetchURL_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	host := mustHost(t, srv.URL)
	fn := fetchURLFunc(fetchGate(t), fetchCfg([]string{"http://" + host}, nil))

	res, err := fn(tool.Context(nil), fetchURLArgs{URL: srv.URL})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.Status != 200 {
		t.Errorf("status = %d, want 200", res.Status)
	}
	if res.Body != `{"ok":true}` {
		t.Errorf("body = %q, want JSON", res.Body)
	}
	if res.ContentType != "application/json" {
		t.Errorf("content_type = %q", res.ContentType)
	}
	if res.Truncated {
		t.Error("unexpected truncation")
	}
}

func TestFetchURL_AllowEmpty_Denied(t *testing.T) {
	fn := fetchURLFunc(fetchGate(t), fetchCfg(nil, nil))
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: "https://example.com"})
	if err == nil || !strings.Contains(err.Error(), "url_scope.allow is empty") {
		t.Errorf("want default-deny error, got: %v", err)
	}
}

func TestFetchURL_HostNotInAllowlist(t *testing.T) {
	fn := fetchURLFunc(fetchGate(t), fetchCfg([]string{"github.com"}, nil))
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: "https://other.com/x"})
	if err == nil || !strings.Contains(err.Error(), "not in allowlist") {
		t.Errorf("want allowlist denial, got: %v", err)
	}
}

func TestFetchURL_DenyBeatsAllow(t *testing.T) {
	fn := fetchURLFunc(fetchGate(t), fetchCfg(
		[]string{"*.example.com"},
		[]string{"evil.example.com"},
	))
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: "https://evil.example.com/"})
	if err == nil || !strings.Contains(err.Error(), "deny pattern") {
		t.Errorf("want deny match, got: %v", err)
	}
}

func TestFetchURL_HTTPSDefaultRejectsPlainHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	// Allowlist entry without http:// prefix → HTTPS only.
	fn := fetchURLFunc(fetchGate(t), fetchCfg([]string{mustHost(t, srv.URL)}, nil))
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "not in allowlist") {
		t.Errorf("want http denial without explicit http:// prefix, got: %v", err)
	}
}

func TestFetchURL_RedirectAllowed(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "landed")
	}))
	defer target.Close()

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer src.Close()

	fn := fetchURLFunc(fetchGate(t), fetchCfg(
		[]string{"http://" + mustHost(t, src.URL), "http://" + mustHost(t, target.URL)},
		nil,
	))
	res, err := fn(tool.Context(nil), fetchURLArgs{URL: src.URL})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.Body != "landed" {
		t.Errorf("body = %q, want %q", res.Body, "landed")
	}
	if res.FinalURL != target.URL {
		t.Errorf("final_url = %q, want %q", res.FinalURL, target.URL)
	}
}

func TestFetchURL_RedirectToDeniedHost(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://denied.example.com/", http.StatusFound)
	}))
	defer src.Close()

	fn := fetchURLFunc(fetchGate(t), fetchCfg(
		[]string{"http://" + mustHost(t, src.URL)},
		nil,
	))
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: src.URL})
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("want redirect denial, got: %v", err)
	}
}

func TestFetchURL_BodyCapTruncates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, strings.Repeat("x", 4096))
	}))
	defer srv.Close()

	cfg := fetchCfg([]string{"http://" + mustHost(t, srv.URL)}, nil)
	cfg.URLScope.MaxBodyBytes = 100
	fn := fetchURLFunc(fetchGate(t), cfg)
	res, err := fn(tool.Context(nil), fetchURLArgs{URL: srv.URL})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !res.Truncated {
		t.Error("want truncated=true")
	}
	if len(res.Body) != 100 {
		t.Errorf("body len = %d, want 100", len(res.Body))
	}
}

func TestFetchURL_BinaryContentSuppressed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0x00, 0x01, 0x02, 0xff})
	}))
	defer srv.Close()

	fn := fetchURLFunc(fetchGate(t), fetchCfg([]string{"http://" + mustHost(t, srv.URL)}, nil))
	res, err := fn(tool.Context(nil), fetchURLArgs{URL: srv.URL})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.Body != "" {
		t.Errorf("body should be empty for binary content, got %q", res.Body)
	}
	if !res.Truncated {
		t.Error("binary content should set truncated=true")
	}
	if res.Bytes != 4 {
		t.Errorf("bytes = %d, want 4 (length is still reported)", res.Bytes)
	}
}

func TestFetchURL_HeaderInjection_EnvExpanded(t *testing.T) {
	t.Setenv("FETCH_TEST_TOKEN", "the-secret")
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	host := mustHost(t, srv.URL)
	cfg := fetchCfg([]string{"http://" + host}, nil)
	cfg.URLScope.Headers = map[string]map[string]string{
		host: {
			"Authorization": "Bearer ${FETCH_TEST_TOKEN}",
			"Accept":        "application/json",
		},
	}
	fn := fetchURLFunc(fetchGate(t), cfg)
	if _, err := fn(tool.Context(nil), fetchURLArgs{URL: srv.URL}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Get("Authorization") != "Bearer the-secret" {
		t.Errorf("Authorization = %q, want expanded", got.Get("Authorization"))
	}
	if got.Get("Accept") != "application/json" {
		t.Errorf("Accept = %q", got.Get("Accept"))
	}
}

func TestFetchURL_HeaderInjection_MostSpecificWins(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	host := mustHost(t, srv.URL)
	cfg := fetchCfg([]string{"http://" + host}, nil)
	cfg.URLScope.Headers = map[string]map[string]string{
		"*":  {"X-Source": "catchall"},
		host: {"X-Source": "specific"},
	}
	fn := fetchURLFunc(fetchGate(t), cfg)
	if _, err := fn(tool.Context(nil), fetchURLArgs{URL: srv.URL}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Get("X-Source") != "specific" {
		t.Errorf("most-specific should win; X-Source = %q", got.Get("X-Source"))
	}
}

func TestFetchURL_EmptyURL(t *testing.T) {
	fn := fetchURLFunc(fetchGate(t), fetchCfg([]string{"*"}, nil))
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: ""})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("want url-required error, got: %v", err)
	}
}

func TestFetchURL_UnsupportedScheme(t *testing.T) {
	fn := fetchURLFunc(fetchGate(t), fetchCfg([]string{"*"}, nil))
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: "ftp://example.com/file"})
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Errorf("want scheme error, got: %v", err)
	}
}

// --- urlMatcher unit tests (no HTTP) -----------------------------------

func TestURLMatcher_SubdomainWildcard(t *testing.T) {
	t.Parallel()
	m := newURLMatcher([]string{"*.example.com"}, nil)

	cases := []struct {
		url       string
		wantAllow bool
	}{
		{"https://api.example.com/x", true},
		{"https://deep.nested.example.com/x", true},
		// Bare apex must be listed separately — the * is for subdomains.
		{"https://example.com/x", false},
		{"https://other.com/x", false},
	}
	for _, c := range cases {
		u, _ := url.Parse(c.url)
		err := m.check(u)
		got := err == nil
		if got != c.wantAllow {
			t.Errorf("%s: allow=%v want %v (err=%v)", c.url, got, c.wantAllow, err)
		}
	}
}

func TestURLMatcher_BareWildcard(t *testing.T) {
	t.Parallel()
	m := newURLMatcher([]string{"*"}, nil)

	for _, host := range []string{"https://github.com/x", "https://anywhere.io/y"} {
		u, _ := url.Parse(host)
		if err := m.check(u); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
	// "*" alone doesn't grant http://.
	u, _ := url.Parse("http://github.com/x")
	if err := m.check(u); err == nil {
		t.Error("bare * should not grant http://")
	}
}

func TestURLMatcher_PortPattern(t *testing.T) {
	t.Parallel()
	m := newURLMatcher([]string{"http://localhost:8080"}, nil)

	u1, _ := url.Parse("http://localhost:8080/")
	if err := m.check(u1); err != nil {
		t.Errorf("matching port: %v", err)
	}
	u2, _ := url.Parse("http://localhost:9090/")
	if err := m.check(u2); err == nil {
		t.Error("port-mismatch should deny")
	}

	m2 := newURLMatcher([]string{"http://localhost:*"}, nil)
	if err := m2.check(u2); err != nil {
		t.Errorf("wildcard port: %v", err)
	}
}

// mustHost extracts host:port from a URL string for use in allowlist
// patterns. httptest URLs always carry a port.
func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}
