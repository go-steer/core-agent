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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
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

// TestFetchURL_Redirect_InjectedHeadersDoNotCrossHosts pins the #385
// fix: a header bundle injected for the ORIGIN host must not ride a
// cross-host redirect. Go's http.Client strips Authorization/Cookie
// on cross-host redirects but forwards custom headers (X-Api-Key),
// so CheckRedirect has to strip operator-injected names itself. Two
// hostnames are mapped onto the same loopback test servers via the
// injectable resolver because header bundles match on host name
// (port-insensitive) — two bare 127.0.0.1 ports couldn't carry
// distinct bundles.
func TestFetchURL_Redirect_InjectedHeadersDoNotCrossHosts(t *testing.T) {
	var gotB http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotB = r.Header.Clone()
		_, _ = fmt.Fprint(w, "landed")
	}))
	defer target.Close()
	portB := mustPort(t, target.URL)

	var gotA http.Header
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotA = r.Header.Clone()
		http.Redirect(w, r, "http://b.internal:"+portB+"/", http.StatusFound)
	}))
	defer src.Close()
	portA := mustPort(t, src.URL)

	cfg := fetchCfg([]string{"http://a.internal:" + portA, "http://b.internal:" + portB}, nil)
	cfg.URLScope.Headers = map[string]map[string]string{
		"a.internal": {"X-Api-Key": "secret-for-a"},
	}
	fn := fetchURLFuncWithResolver(fetchGate(t), cfg, staticResolver(map[string][]string{
		"a.internal": {"127.0.0.1"},
		"b.internal": {"127.0.0.1"},
	}))

	res, err := fn(tool.Context(nil), fetchURLArgs{URL: "http://a.internal:" + portA + "/"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.Body != "landed" {
		t.Errorf("body = %q, want %q", res.Body, "landed")
	}
	if gotA.Get("X-Api-Key") != "secret-for-a" {
		t.Errorf("origin host A should receive its own bundle header, got %q", gotA.Get("X-Api-Key"))
	}
	if leaked := gotB.Get("X-Api-Key"); leaked != "" {
		t.Errorf("host A's injected X-Api-Key leaked to host B across the redirect: %q", leaked)
	}
}

// TestFetchURL_Redirect_TargetHostBundleApplies is the positive half:
// when the redirect TARGET has its own header bundle, that bundle is
// applied to the redirected request — the recomputation swaps host
// A's credentials for host B's rather than just dropping everything.
func TestFetchURL_Redirect_TargetHostBundleApplies(t *testing.T) {
	var gotB http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotB = r.Header.Clone()
		_, _ = fmt.Fprint(w, "landed")
	}))
	defer target.Close()
	portB := mustPort(t, target.URL)

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://b.internal:"+portB+"/", http.StatusFound)
	}))
	defer src.Close()
	portA := mustPort(t, src.URL)

	cfg := fetchCfg([]string{"http://a.internal:" + portA, "http://b.internal:" + portB}, nil)
	cfg.URLScope.Headers = map[string]map[string]string{
		"a.internal": {"X-Api-Key": "secret-for-a"},
		"b.internal": {"X-Api-Key": "secret-for-b", "X-B-Extra": "yes"},
	}
	fn := fetchURLFuncWithResolver(fetchGate(t), cfg, staticResolver(map[string][]string{
		"a.internal": {"127.0.0.1"},
		"b.internal": {"127.0.0.1"},
	}))

	if _, err := fn(tool.Context(nil), fetchURLArgs{URL: "http://a.internal:" + portA + "/"}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := gotB.Get("X-Api-Key"); got != "secret-for-b" {
		t.Errorf("host B should receive ITS bundle's X-Api-Key, got %q", got)
	}
	if got := gotB.Get("X-B-Extra"); got != "yes" {
		t.Errorf("host B's own extra header should arrive, got %q", got)
	}
}

// mustPort extracts the port from a URL string.
func mustPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Port()
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

// --- SSRF guard tests ---------------------------------------------------

// staticResolver returns a fetchResolver serving a fixed hostname→IPs
// map; unknown hosts error like NXDOMAIN.
func staticResolver(m map[string][]string) fetchResolver {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		ips, ok := m[host]
		if !ok {
			return nil, fmt.Errorf("lookup %s: no such host", host)
		}
		out := make([]netip.Addr, 0, len(ips))
		for _, s := range ips {
			out = append(out, netip.MustParseAddr(s))
		}
		return out, nil
	}
}

func TestFetchURL_MetadataLiteralIP_BlockedDespiteWildcard(t *testing.T) {
	t.Parallel()
	fn := fetchURLFunc(fetchGate(t), fetchCfg([]string{"http://*"}, nil))
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: "http://169.254.169.254/latest/meta-data/"})
	if err == nil || !strings.Contains(err.Error(), "link-local/metadata") {
		t.Errorf("want metadata block, got: %v", err)
	}
}

func TestFetchURL_MetadataLiteralIP_BlockedDespiteExactAllow(t *testing.T) {
	t.Parallel()
	// Even an exact-host allowlist entry does not unlock the
	// metadata ranges — only allow_metadata_endpoints does.
	fn := fetchURLFunc(fetchGate(t), fetchCfg([]string{"http://169.254.169.254"}, nil))
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: "http://169.254.169.254/latest/meta-data/"})
	if err == nil || !strings.Contains(err.Error(), "link-local/metadata") {
		t.Errorf("want metadata block, got: %v", err)
	}
}

func TestFetchURL_MetadataHostname_Blocked(t *testing.T) {
	t.Parallel()
	// A hostname under attacker DNS control resolving to the
	// metadata IP is caught at dial time, wildcard allow or not.
	fn := fetchURLFuncWithResolver(
		fetchGate(t),
		fetchCfg([]string{"http://*"}, nil),
		staticResolver(map[string][]string{"metadata.internal": {"169.254.169.254"}}),
	)
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: "http://metadata.internal/latest/meta-data/"})
	if err == nil || !strings.Contains(err.Error(), "link-local/metadata") {
		t.Errorf("want metadata block, got: %v", err)
	}
}

func TestFetchURL_PrivateIP_WildcardAllow_Blocked(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should never be reached")
	}))
	defer srv.Close()

	// Wildcard allowlist entry does NOT unlock loopback/private.
	fn := fetchURLFunc(fetchGate(t), fetchCfg([]string{"http://*"}, nil))
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "loopback/private") {
		t.Errorf("want private-range block, got: %v", err)
	}
}

func TestFetchURL_PrivateIP_ExactHostAllow_PinnedDial(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "internal ok")
	}))
	defer srv.Close()

	// The URL uses a hostname real DNS cannot resolve; the fake
	// resolver maps it to the test server's loopback IP. Success
	// therefore proves two things at once: an exact-host allowlist
	// entry unlocks the private range, and the dial went to the
	// pinned resolver-provided IP (not a second resolution).
	u, _ := url.Parse(srv.URL)
	host := "app.internal:" + u.Port()
	fn := fetchURLFuncWithResolver(
		fetchGate(t),
		fetchCfg([]string{"http://" + host}, nil),
		staticResolver(map[string][]string{"app.internal": {u.Hostname()}}),
	)
	res, err := fn(tool.Context(nil), fetchURLArgs{URL: "http://" + host + "/"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.Body != "internal ok" {
		t.Errorf("body = %q, want %q", res.Body, "internal ok")
	}
}

func TestFetchURL_Rebinding_PrivateResolution_WildcardBlocked(t *testing.T) {
	t.Parallel()
	// Rebinding simulation: the resolver hands back a loopback IP
	// for a public-looking host that only a wildcard entry allows.
	fn := fetchURLFuncWithResolver(
		fetchGate(t),
		fetchCfg([]string{"http://*"}, nil),
		staticResolver(map[string][]string{"rebind.example.net": {"127.0.0.1"}}),
	)
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: "http://rebind.example.net/"})
	if err == nil || !strings.Contains(err.Error(), "loopback/private") {
		t.Errorf("want private-range block, got: %v", err)
	}
}

func TestFetchURL_MixedResolution_AnyBadIPRejects(t *testing.T) {
	t.Parallel()
	// A host mixing a public IP with the metadata IP is rejected
	// outright — even with an exact-host allowlist entry.
	fn := fetchURLFuncWithResolver(
		fetchGate(t),
		fetchCfg([]string{"http://mixed.example.net"}, nil),
		staticResolver(map[string][]string{"mixed.example.net": {"93.184.216.34", "169.254.169.254"}}),
	)
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: "http://mixed.example.net/"})
	if err == nil || !strings.Contains(err.Error(), "link-local/metadata") {
		t.Errorf("want metadata block, got: %v", err)
	}
}

func TestFetchURL_RedirectToMetadataIP_Blocked(t *testing.T) {
	t.Parallel()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer src.Close()

	// The wildcard entry lets the redirect target pass the host
	// allowlist; the IP guard must still stop it.
	fn := fetchURLFunc(fetchGate(t), fetchCfg(
		[]string{"http://" + mustHost(t, src.URL), "http://*"},
		nil,
	))
	_, err := fn(tool.Context(nil), fetchURLArgs{URL: src.URL})
	if err == nil || !strings.Contains(err.Error(), "link-local/metadata") {
		t.Errorf("want metadata block on redirect, got: %v", err)
	}
}

// ctxToolContext adapts a plain context.Context into the tool.Context
// interface for tests that need cancellation. Only the context methods
// are backed; everything else panics via the nil embedded interface.
type ctxToolContext struct {
	tool.Context
	ctx context.Context
}

func (c ctxToolContext) Deadline() (time.Time, bool) { return c.ctx.Deadline() }
func (c ctxToolContext) Done() <-chan struct{}       { return c.ctx.Done() }
func (c ctxToolContext) Err() error                  { return c.ctx.Err() }
func (c ctxToolContext) Value(key any) any           { return c.ctx.Value(key) }

func TestFetchURL_ContextCancellationAborts(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	fn := fetchURLFunc(fetchGate(t), fetchCfg([]string{"http://" + mustHost(t, srv.URL)}, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := fn(ctxToolContext{ctx: ctx}, fetchURLArgs{URL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("want context-deadline error, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation took %v; the tool ctx is not threaded into the request", elapsed)
	}
}

func TestSSRFGuard_CheckAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		addr          string
		exactHost     bool
		allowMetadata bool
		wantErr       string // "" = allowed
	}{
		{"public v4", "93.184.216.34", false, false, ""},
		{"public v6", "2606:2800:220:1::1", false, false, ""},
		{"metadata v4", "169.254.169.254", false, false, "link-local/metadata"},
		{"metadata v4 exact host does not unlock", "169.254.169.254", true, false, "link-local/metadata"},
		{"metadata v4 opt-in flag unlocks", "169.254.169.254", false, true, ""},
		{"link-local v4", "169.254.1.1", false, false, "link-local/metadata"},
		{"link-local v6", "fe80::1", false, false, "link-local/metadata"},
		{"aws imds v6", "fd00:ec2::254", true, false, "link-local/metadata"},
		{"aws imds v6 opt-in flag unlocks", "fd00:ec2::254", true, true, ""},
		{"loopback v4", "127.0.0.1", false, false, "loopback/private"},
		{"loopback v4 exact host unlocks", "127.0.0.1", true, false, ""},
		{"loopback v6", "::1", false, false, "loopback/private"},
		{"mapped loopback unmapped first", "::ffff:127.0.0.1", false, false, "loopback/private"},
		{"rfc1918 10/8", "10.1.2.3", false, false, "loopback/private"},
		{"rfc1918 172.16/12", "172.20.0.1", false, false, "loopback/private"},
		{"rfc1918 192.168/16", "192.168.1.1", false, false, "loopback/private"},
		{"cgnat 100.64/10", "100.64.0.1", false, false, "loopback/private"},
		{"ula fc00::/7", "fc00::1", false, false, "loopback/private"},
		{"ula exact host unlocks", "fc00::1", true, false, ""},
		{"rfc1918 exact host unlocks", "10.1.2.3", true, false, ""},
		// #428 additions.
		{"ietf special-purpose 192.0.0.0/24 metadata-adjacent", "192.0.0.192", false, false, "link-local/metadata"},
		{"192.0.0.0/24 exact host does not unlock", "192.0.0.192", true, false, "link-local/metadata"},
		{"192.0.0.0/24 opt-in flag unlocks", "192.0.0.192", false, true, ""},
		{"unspecified v4 0.0.0.0", "0.0.0.0", false, false, "loopback/private"},
		{"this-network 0/8", "0.1.2.3", false, false, "loopback/private"},
		{"unspecified v6 ::", "::", false, false, "loopback/private"},
		{"deprecated v4-compatible embedding", "::10.1.2.3", false, false, "loopback/private"},
		{"nat64 well-known embedding private v4", "64:ff9b::10.0.0.1", false, false, "loopback/private"},
		{"nat64 well-known embedding public v4", "64:ff9b::5db8:d822", false, false, "loopback/private"},
		{"nat64 local-use prefix", "64:ff9b:1::1", false, false, "loopback/private"},
		{"nat64 exact host unlocks", "64:ff9b::10.0.0.1", true, false, ""},
		{"benchmarking 198.18/15", "198.19.255.1", false, false, "loopback/private"},
		{"limited broadcast", "255.255.255.255", false, false, "loopback/private"},
		{"multicast v4", "224.0.0.251", false, false, "loopback/private"},
		{"multicast v6", "ff02::fb", false, false, "loopback/private"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			g := &ssrfGuard{allowMetadata: c.allowMetadata}
			err := g.checkAddr(netip.MustParseAddr(c.addr), "host.test", c.exactHost)
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("checkAddr(%s): unexpected error: %v", c.addr, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("checkAddr(%s): got %v, want error containing %q", c.addr, err, c.wantErr)
			}
		})
	}
}

func TestURLMatcher_AllowsExactHost(t *testing.T) {
	t.Parallel()
	m := newURLMatcher([]string{
		"api.github.com",
		"http://localhost:8080",
		"*.svc.cluster.local",
		"*",
	}, nil)

	cases := []struct {
		host, port string
		want       bool
	}{
		{"api.github.com", "443", true},
		{"API.GITHUB.COM", "443", true},         // case-insensitive
		{"localhost", "8080", true},             // exact host, matching port
		{"localhost", "9090", false},            // exact host, wrong port
		{"db.svc.cluster.local", "5432", false}, // wildcard entry never counts
		{"anything.example.com", "443", false},  // bare * never counts
	}
	for _, c := range cases {
		if got := m.allowsExactHost(c.host, c.port); got != c.want {
			t.Errorf("allowsExactHost(%q, %q) = %v, want %v", c.host, c.port, got, c.want)
		}
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

// TestProxyFuncFor pins the url_scope.proxy → transport.Proxy mapping
// (#429): default is NO proxy even when HTTP_PROXY is set in the
// environment (with a proxy in the path, hostname targets resolve at
// the proxy, outside the SSRF guard — so proxying must be explicit);
// "env" opts back in; a fixed URL routes everything there.
func TestProxyFuncFor(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://ambient-proxy.corp:3128")
	t.Setenv("HTTPS_PROXY", "http://ambient-proxy.corp:3128")

	req, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)

	if fn := proxyFuncFor(""); fn != nil {
		t.Errorf("proxyFuncFor(\"\") = non-nil; ambient env proxies must be ignored by default")
	}

	fn := proxyFuncFor("env")
	if fn == nil {
		t.Fatal("proxyFuncFor(\"env\") = nil, want ProxyFromEnvironment")
	}
	u, err := fn(req)
	if err != nil || u == nil || u.Host != "ambient-proxy.corp:3128" {
		t.Errorf("proxyFuncFor(\"env\")(req) = %v, %v; want the ambient proxy", u, err)
	}

	fn = proxyFuncFor("http://fixed-proxy.corp:8080")
	if fn == nil {
		t.Fatal("proxyFuncFor(fixed) = nil")
	}
	u, err = fn(req)
	if err != nil || u == nil || u.Host != "fixed-proxy.corp:8080" {
		t.Errorf("proxyFuncFor(fixed)(req) = %v, %v; want the fixed proxy", u, err)
	}

	// Defense-in-depth: a malformed value that slipped past
	// config.Validate falls back to no-proxy, not a guess.
	if fn := proxyFuncFor("://bad"); fn != nil {
		t.Errorf("proxyFuncFor(malformed) = non-nil, want nil (no proxy)")
	}
}

// TestFetchURL_TransportIgnoresAmbientProxyByDefault is the
// integration pin for the #429 default: the constructed tool's
// transport must carry a nil Proxy func when url_scope.proxy is
// unset, so a poisoned/ambient HTTP_PROXY cannot pull hostname
// resolution out of the SSRF guard's dial path.
func TestFetchURL_TransportIgnoresAmbientProxyByDefault(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://ambient-proxy.corp:3128")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("direct"))
	}))
	t.Cleanup(srv.Close)

	fn := fetchURLFunc(fetchGate(t), fetchCfg([]string{srv.URL}, nil))

	// The test server listens on 127.0.0.1 with the exact host:port
	// allowlisted, so the only way this fetch fails is if the
	// transport tried to route through the (nonexistent) ambient
	// proxy instead of dialing direct.
	out, err := fn(tool.Context(nil), fetchURLArgs{URL: srv.URL})
	if err != nil {
		t.Fatalf("fetch via ambient-proxy env: %v (transport should dial direct)", err)
	}
	if out.Body != "direct" {
		t.Errorf("body = %q, want %q", out.Body, "direct")
	}
}
