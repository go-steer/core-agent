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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// fetch_url defaults. Overridden by URLScopeConfig.MaxBodyBytes /
// TimeoutSeconds / by the per-call max_bytes arg.
const (
	fetchURLDefaultMaxBodyBytes = 64 * 1024
	fetchURLDefaultTimeout      = 30 * time.Second
	fetchURLMaxRedirects        = 5
)

type fetchURLArgs struct {
	URL      string `json:"url" jsonschema:"fully-qualified URL to fetch via HTTP GET (e.g. https://api.github.com/repos/X/issues/1). Must match an allow-list pattern in config.url_scope.allow; HTTPS unless the operator explicitly allowed http:// patterns."`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"body size cap in bytes. Default 65536. Capped by url_scope.max_body_bytes."`
}

type fetchURLResult struct {
	URL         string `json:"url"`
	FinalURL    string `json:"final_url"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Bytes       int    `json:"bytes"`
	Truncated   bool   `json:"truncated"`
	Body        string `json:"body"`
}

// NewFetchURLTool returns the fetch_url built-in. Only meaningful
// when cfg.URLScope.Allow is non-empty — with no allowlist, every
// fetch will be denied. The caller (builtins.go) should skip
// registering this tool when no allowlist is configured rather than
// register-then-refuse, but the tool itself is safe either way.
func NewFetchURLTool(gate *permissions.Gate, cfg *config.Config) tool.Tool {
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "fetch_url",
			Description: "Fetch a URL via HTTP GET. Returns body, status, content-type, and final-URL after redirects. URLs must be in the operator's url_scope.allow list (typical: GitHub API, GCP APIs, internal cluster services). HTTPS by default; http:// only when explicitly allowed. Use this instead of `bash curl` so the URL + status land structured in the eventlog and the per-host header config can inject auth tokens for you. Body is capped (default 64KB) — pass max_bytes to override up to url_scope.max_body_bytes. Each redirect target is re-checked against the allowlist; a redirect to a denied host is an error, not a silent follow. Link-local/cloud-metadata addresses (169.254.0.0/16, fe80::/10) are always refused; loopback and private-range addresses are refused unless the exact host appears in url_scope.allow (a wildcard entry does not unlock them).",
		},
		fetchURLFunc(gate, cfg),
	)
	if err != nil {
		panic("tools: NewFetchURLTool: " + err.Error())
	}
	return t
}

// fetchResolver is the DNS seam: it resolves a hostname to the set
// of IPs the guarded dialer is allowed to consider. Tests inject a
// fake; production uses net.DefaultResolver.
type fetchResolver func(ctx context.Context, host string) ([]netip.Addr, error)

func defaultFetchResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// fetchURLFunc is the handler, extracted so tests can drive it
// without going through ADK's functiontool wrapper.
func fetchURLFunc(gate *permissions.Gate, cfg *config.Config) functiontool.Func[fetchURLArgs, fetchURLResult] {
	return fetchURLFuncWithResolver(gate, cfg, nil)
}

// fetchURLFuncWithResolver is fetchURLFunc with an injectable DNS
// resolver. resolve == nil means net.DefaultResolver.
func fetchURLFuncWithResolver(gate *permissions.Gate, cfg *config.Config, resolve fetchResolver) functiontool.Func[fetchURLArgs, fetchURLResult] {
	scope := cfg.URLScope
	matcher := newURLMatcher(scope.Allow, scope.Deny)
	timeout := fetchURLDefaultTimeout
	if scope.TimeoutSeconds > 0 {
		timeout = time.Duration(scope.TimeoutSeconds) * time.Second
	}
	scopeCap := scope.MaxBodyBytes
	if scopeCap <= 0 {
		scopeCap = fetchURLDefaultMaxBodyBytes
	}
	if resolve == nil {
		resolve = defaultFetchResolver
	}
	guard := &ssrfGuard{
		matcher:       matcher,
		allowMetadata: scope.AllowMetadataEndpoints,
		resolve:       resolve,
	}
	// One transport for the tool's lifetime: every connection —
	// initial request and each redirect hop alike — dials through
	// the guard, which resolves the host once, validates every
	// returned IP, and dials only a vetted IP (SSRF / DNS-rebinding
	// defense; see ssrfGuard).
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = guard.dialContext
	// Proxying is explicit-config only (#429). DefaultTransport's
	// ProxyFromEnvironment would silently route through
	// HTTP_PROXY/HTTPS_PROXY, and with a proxy in the path the
	// guarded dial validates/pins the PROXY's address — hostname
	// targets are resolved at the proxy, outside the SSRF guard.
	// So ambient env proxies are ignored; url_scope.proxy: "env"
	// opts back in, and a fixed proxy URL routes everything there.
	// In both proxied modes the operator delegates private/metadata
	// SSRF policy for hostname targets to the proxy; literal-IP
	// targets stay screened locally (checkURL, every redirect hop).
	transport.Proxy = proxyFuncFor(scope.Proxy)
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= fetchURLMaxRedirects {
				return fmt.Errorf("fetch_url: stopped after %d redirects", fetchURLMaxRedirects)
			}
			if err := matcher.check(req.URL); err != nil {
				return fmt.Errorf("fetch_url: redirect %s", err)
			}
			// Early literal-IP screen for a clearer error; the
			// dial-time guard remains authoritative for hostnames.
			if err := guard.checkURL(req.URL); err != nil {
				return fmt.Errorf("fetch_url: redirect %s", err)
			}
			// Recompute operator-injected header entitlement for the
			// redirect target (#385). Go strips Authorization/Cookie
			// on a cross-host redirect but forwards every CUSTOM
			// header from the original request — including auth
			// headers injected from a url_scope.headers bundle
			// (X-Api-Key and friends), which would leak host A's
			// credential to host B. Strip every header name any
			// bundle can inject, then re-apply the bundle matching
			// the NEW host, if one exists. Only operator-injected
			// names are touched; Go's own sensitive-header handling
			// and all other request headers are left alone.
			stripInjectedHeaders(req, scope.Headers)
			injectHeaders(req, req.URL.Host, scope.Headers)
			return nil
		},
	}
	return func(ctx tool.Context, in fetchURLArgs) (fetchURLResult, error) {
		if in.URL == "" {
			return fetchURLResult{}, errors.New("fetch_url: url is required")
		}
		parsed, err := url.Parse(in.URL)
		if err != nil {
			return fetchURLResult{}, fmt.Errorf("fetch_url: parse url: %w", err)
		}
		if parsed.Host == "" {
			return fetchURLResult{}, fmt.Errorf("fetch_url: url has no host: %q", in.URL)
		}

		// Gate first — operators can lock down per-host via
		// permissions.allow: ["fetch_url:github.com/*"] etc.
		// Key passes the URL as the gate sees it; pattern-matchers
		// on the gate side do their own globbing.
		if err := gate.CheckGeneric(ctx, "fetch_url", in.URL); err != nil {
			return fetchURLResult{}, err
		}

		if err := matcher.check(parsed); err != nil {
			return fetchURLResult{}, err
		}
		// Early literal-IP screen for a clearer error; the dial-time
		// guard remains authoritative for hostnames.
		if err := guard.checkURL(parsed); err != nil {
			return fetchURLResult{}, fmt.Errorf("fetch_url: %w", err)
		}

		cap := in.MaxBytes
		if cap <= 0 || cap > scopeCap {
			cap = scopeCap
		}

		// Parent the request on the inbound tool ctx (not
		// context.Background) so the turn-level cancel signal —
		// /interrupt, daemon shutdown — aborts an in-flight fetch.
		// tool.Context is an interface; some tests pass nil. Fall
		// back to Background in that case.
		parent := context.Context(ctx)
		if parent == nil {
			parent = context.Background()
		}
		req, err := http.NewRequestWithContext(parent, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return fetchURLResult{}, fmt.Errorf("fetch_url: build request: %w", err)
		}
		injectHeaders(req, parsed.Host, scope.Headers)

		resp, err := client.Do(req)
		if err != nil {
			return fetchURLResult{}, fmt.Errorf("fetch_url: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		contentType := resp.Header.Get("Content-Type")
		bodyBytes, truncated, err := readBodyCapped(resp.Body, cap)
		if err != nil {
			return fetchURLResult{}, fmt.Errorf("fetch_url: read body: %w", err)
		}

		// Don't return arbitrary binary bytes inline to the model;
		// it'll spew control characters and waste prompt cache.
		// JSON and text are returned as-is; everything else is
		// reported as truncated with an empty body so the model
		// gets the metadata (status, content-type, size) but not
		// the bytes.
		out := string(bodyBytes)
		if !isTextContentType(contentType) {
			out = ""
			truncated = true
		}

		return fetchURLResult{
			URL:         in.URL,
			FinalURL:    resp.Request.URL.String(),
			Status:      resp.StatusCode,
			ContentType: contentType,
			Bytes:       len(bodyBytes),
			Truncated:   truncated,
			Body:        out,
		}, nil
	}
}

// readBodyCapped reads up to cap bytes. If the underlying body has
// more, sets truncated=true; never returns more than cap bytes.
func readBodyCapped(r io.Reader, cap int) ([]byte, bool, error) {
	// Read one byte past the cap to detect overflow without
	// pre-reading the whole body.
	buf, err := io.ReadAll(io.LimitReader(r, int64(cap)+1))
	if err != nil {
		return nil, false, err
	}
	if len(buf) > cap {
		return buf[:cap], true, nil
	}
	return buf, false, nil
}

// isTextContentType returns true for content types we're willing to
// surface as a body string. Anything else (binary, octet-stream,
// images, video, etc.) is returned with body="" and truncated=true.
func isTextContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		// No content-type header → assume text-ish; many APIs omit it.
		return true
	}
	// Strip parameters (e.g. "text/html; charset=utf-8" → "text/html").
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch {
	case strings.HasPrefix(ct, "text/"):
		return true
	case ct == "application/json", strings.HasSuffix(ct, "+json"):
		return true
	case ct == "application/xml", strings.HasSuffix(ct, "+xml"):
		return true
	case ct == "application/javascript", ct == "application/ecmascript":
		return true
	case ct == "application/yaml", strings.HasSuffix(ct, "+yaml"):
		return true
	default:
		return false
	}
}

// injectHeaders walks scope.Headers picking the most-specific matching
// host pattern (longest pattern wins; exact match beats wildcard) and
// applies its header bundle to req. Values pass through os.ExpandEnv
// so "Bearer ${GITHUB_TOKEN}" picks up rotated env at request time.
func injectHeaders(req *http.Request, host string, headers map[string]map[string]string) {
	if len(headers) == 0 {
		return
	}
	var bestPattern string
	for pattern := range headers {
		if !hostMatchesHeaderPattern(host, pattern) {
			continue
		}
		// Prefer the longest pattern (more specific wins). Exact
		// match (no '*') beats any wildcard at the same length.
		if better(bestPattern, pattern) {
			bestPattern = pattern
		}
	}
	if bestPattern == "" {
		return
	}
	for name, value := range headers[bestPattern] {
		req.Header.Set(name, os.ExpandEnv(value))
	}
}

// stripInjectedHeaders removes from req every header name that ANY
// url_scope.headers bundle can inject, regardless of which host's
// bundle set it. Called from CheckRedirect before re-applying the
// redirect target's own bundle, so a credential injected for host A
// never rides a redirect to host B (#385). Header names are
// canonicalized by http.Header.Del, matching how injectHeaders set
// them via http.Header.Set.
func stripInjectedHeaders(req *http.Request, headers map[string]map[string]string) {
	for _, bundle := range headers {
		for name := range bundle {
			req.Header.Del(name)
		}
	}
}

func better(current, candidate string) bool {
	if current == "" {
		return true
	}
	// Exact (no wildcard) beats wildcard.
	curWild := strings.Contains(current, "*")
	candWild := strings.Contains(candidate, "*")
	if curWild != candWild {
		return curWild // candidate non-wildcard beats current wildcard
	}
	// Otherwise longer pattern wins.
	return len(candidate) > len(current)
}

// hostMatchesHeaderPattern: pattern is a bare host pattern (no scheme;
// trailing ":<port>" is stripped if present). Supports leading "*."
// for subdomain wildcard or bare "*" for any host. Tolerant of either
// form on both sides — common to copy-paste a host:port from an
// allowlist entry into the header map.
func hostMatchesHeaderPattern(host, pattern string) bool {
	stripPort := func(s string) string {
		if i := strings.LastIndex(s, ":"); i > 0 && !strings.Contains(s[i:], "]") {
			return s[:i]
		}
		return s
	}
	host = strings.ToLower(stripPort(host))
	pattern = strings.ToLower(stripPort(pattern))
	if pattern == host {
		return true
	}
	if pattern == "*" {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		return strings.HasSuffix(host, suffix) && host != suffix[1:]
	}
	return false
}

// urlMatcher is the per-call allow/deny check for fetch_url. Patterns
// are compiled once at tool-construction time so each call is cheap.
type urlMatcher struct {
	allow []hostPattern
	deny  []hostPattern
}

func newURLMatcher(allow, deny []string) *urlMatcher {
	m := &urlMatcher{}
	for _, p := range allow {
		m.allow = append(m.allow, parseHostPattern(p))
	}
	for _, p := range deny {
		m.deny = append(m.deny, parseHostPattern(p))
	}
	return m
}

// check returns nil if the URL is allowed, otherwise a model-readable
// error describing why. Caller is responsible for closing over the
// configured Allow/Deny patterns.
func (m *urlMatcher) check(u *url.URL) error {
	if len(m.allow) == 0 {
		return errors.New("url_scope.allow is empty: fetch_url denies every URL until the operator adds an allowlist entry")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("url_scope: unsupported scheme %q (only http/https are supported)", u.Scheme)
	}
	for _, p := range m.deny {
		if p.matches(u) {
			return fmt.Errorf("url_scope: %s matches a deny pattern", u.Host)
		}
	}
	for _, p := range m.allow {
		if p.matches(u) {
			return nil
		}
	}
	return fmt.Errorf("url_scope: %s://%s not in allowlist", scheme, u.Host)
}

// hostPattern is a parsed allow/deny entry. Default scheme is HTTPS
// (allowHTTP=false); the "http://" prefix flips it. Default port
// is "any". Host pattern supports leading "*." for subdomain
// wildcard or bare "*" for any host.
type hostPattern struct {
	host      string // "github.com", "*.example.com", or "*"
	port      string // "" = any, "*" = any, else literal
	allowHTTP bool   // true if pattern carried http:// prefix
}

func parseHostPattern(raw string) hostPattern {
	p := hostPattern{}
	s := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(s, "http://"):
		s = strings.TrimPrefix(s, "http://")
		p.allowHTTP = true
	case strings.HasPrefix(s, "https://"):
		s = strings.TrimPrefix(s, "https://")
	}
	// Split optional :port.
	if i := strings.LastIndex(s, ":"); i >= 0 {
		p.host = s[:i]
		p.port = s[i+1:]
	} else {
		p.host = s
	}
	p.host = strings.ToLower(p.host)
	return p
}

func (p hostPattern) matches(u *url.URL) bool {
	scheme := strings.ToLower(u.Scheme)
	if scheme == "http" && !p.allowHTTP {
		return false
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if !matchHost(host, p.host) {
		return false
	}
	if p.port != "" && p.port != "*" && p.port != port {
		return false
	}
	return true
}

func matchHost(host, pattern string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if pattern == host {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		// "*.example.com" matches "foo.example.com" but NOT
		// bare "example.com" itself (intentional — wildcard is
		// for subdomains, exact host should be listed separately).
		return strings.HasSuffix(host, suffix) && host != suffix[1:]
	}
	return false
}

// allowsExactHost reports whether an allow entry names this host
// exactly — no wildcard in the host part. An exact entry is the
// operator explicitly opting THIS host in, which is what unlocks
// loopback/private-range destinations for it; a wildcard/broad entry
// ("*", "*.svc.cluster.local") deliberately does not. The port must
// still satisfy the entry's port pattern ("" / "*" = any). Scheme is
// not consulted here — scheme enforcement already happened in
// urlMatcher.check; this is purely "did the operator name the host".
func (m *urlMatcher) allowsExactHost(host, port string) bool {
	host = strings.ToLower(host)
	for _, p := range m.allow {
		if p.host == "" || strings.Contains(p.host, "*") {
			continue
		}
		if p.host != host {
			continue
		}
		if p.port != "" && p.port != "*" && p.port != port {
			continue
		}
		return true
	}
	return false
}

// --- SSRF guard ---------------------------------------------------------
//
// The allowlist above matches host *names*; the guard below vets the
// *addresses* those names resolve to, at dial time. Doing the check
// inside DialContext closes the classic TOCTOU/DNS-rebinding gap: the
// host is resolved exactly once, every returned IP is validated, and
// the connection is dialed to one of those same vetted IPs (never a
// second, attacker-controlled resolution). TLS SNI and the Host
// header are untouched — the transport still sees the original
// hostname; only the TCP dial target is pinned. Every redirect hop
// opens its connection through the same path, so each hop is
// re-validated and re-pinned.

// fetchAlwaysBlockedRanges are hard-blocked in every permission mode
// (including yolo) regardless of the allowlist: link-local ranges,
// which include the cloud metadata services (169.254.169.254, AWS
// IMDSv6 fd00:ec2::254). The only opt-out is the explicit
// url_scope.allow_metadata_endpoints config flag.
var fetchAlwaysBlockedRanges = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),    // IPv4 link-local incl. 169.254.169.254
	netip.MustParsePrefix("fe80::/10"),         // IPv6 link-local
	netip.MustParsePrefix("fd00:ec2::254/128"), // AWS IMDS IPv6
	// IETF protocol assignments (RFC 6890). Metadata-adjacent: some
	// cloud environments serve their metadata endpoint at
	// 192.0.0.192, so the whole special-purpose /24 gets the same
	// hard-block-with-flag-opt-out treatment as link-local (#428).
	netip.MustParsePrefix("192.0.0.0/24"),
}

// fetchPrivateRanges are blocked unless the request host is named by
// an exact (non-wildcard) allowlist entry — the operator explicitly
// opting that host in unlocks its private destination; a broad
// wildcard entry does not.
var fetchPrivateRanges = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),    // IPv4 loopback
	netip.MustParsePrefix("::1/128"),        // IPv6 loopback
	netip.MustParsePrefix("10.0.0.0/8"),     // RFC1918
	netip.MustParsePrefix("172.16.0.0/12"),  // RFC1918
	netip.MustParsePrefix("192.168.0.0/16"), // RFC1918
	netip.MustParsePrefix("100.64.0.0/10"),  // CGNAT
	netip.MustParsePrefix("fc00::/7"),       // IPv6 ULA
	// Special-purpose ranges beyond the original #375 policy set
	// (#428). Same tier as RFC1918: an exact-host allowlist entry
	// unlocks them, wildcards do not.
	netip.MustParsePrefix("0.0.0.0/8"),          // "this network"; 0.0.0.0 reaches loopback on Linux
	netip.MustParsePrefix("::/96"),              // IPv6 unspecified (::) + deprecated IPv4-compatible embedding
	netip.MustParsePrefix("64:ff9b::/96"),       // NAT64 well-known prefix — can embed a private IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"),     // NAT64 local-use prefix (RFC 8215)
	netip.MustParsePrefix("198.18.0.0/15"),      // benchmarking (RFC 2544)
	netip.MustParsePrefix("255.255.255.255/32"), // limited broadcast
	netip.MustParsePrefix("224.0.0.0/4"),        // IPv4 multicast
	netip.MustParsePrefix("ff00::/8"),           // IPv6 multicast
}

// proxyFuncFor maps url_scope.proxy onto an http.Transport.Proxy
// func (#429):
//
//	""    → nil (no proxy; ambient HTTP_PROXY/HTTPS_PROXY ignored)
//	"env" → http.ProxyFromEnvironment (explicit operator opt-in)
//	<url> → fixed proxy for every request
//
// A malformed fixed URL is rejected by config.Validate at load time;
// this parse is a defense-in-depth backstop for hand-constructed
// configs and falls back to no-proxy rather than guessing.
func proxyFuncFor(setting string) func(*http.Request) (*url.URL, error) {
	switch setting {
	case "":
		return nil
	case "env":
		return http.ProxyFromEnvironment
	default:
		u, err := url.Parse(setting)
		if err != nil || u.Host == "" {
			return nil
		}
		return http.ProxyURL(u)
	}
}

// ssrfGuard vets destination IPs and pins the vetted resolution
// through to the dial. It backs the tool's http.Transport.DialContext.
type ssrfGuard struct {
	matcher       *urlMatcher
	allowMetadata bool
	resolve       fetchResolver
}

// checkAddr validates one candidate address. exactHost says whether
// the request host is named by an exact allowlist entry (which
// unlocks the private ranges, never the metadata ranges).
func (g *ssrfGuard) checkAddr(addr netip.Addr, host string, exactHost bool) error {
	a := addr.Unmap() // ::ffff:127.0.0.1 → 127.0.0.1
	if !g.allowMetadata {
		for _, p := range fetchAlwaysBlockedRanges {
			if p.Contains(a) {
				return fmt.Errorf("url_scope: %s resolves to %s, a link-local/metadata address; blocked in all modes (url_scope.allow_metadata_endpoints is the only opt-out)", host, a)
			}
		}
	}
	if exactHost {
		return nil
	}
	for _, p := range fetchPrivateRanges {
		if p.Contains(a) {
			return fmt.Errorf("url_scope: %s resolves to %s, a loopback/private address; blocked unless the exact host is listed in url_scope.allow (wildcard entries do not unlock private ranges)", host, a)
		}
	}
	return nil
}

// checkURL screens a URL whose host is an IP literal, so an obviously
// blocked target fails with a direct error before any dial. Hostname
// targets pass through — they are resolved and vetted in dialContext.
func (g *ssrfGuard) checkURL(u *url.URL) error {
	host := u.Hostname()
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil // not an IP literal; dialContext handles it
	}
	port := u.Port()
	if port == "" {
		if strings.EqualFold(u.Scheme, "http") {
			port = "80"
		} else {
			port = "443"
		}
	}
	return g.checkAddr(addr, host, g.matcher.allowsExactHost(host, port))
}

// dialContext resolves addr's host once, validates every returned IP
// (any bad IP rejects the whole dial), then dials only the vetted
// IPs. This is the authoritative SSRF check — it runs for the initial
// request and for every redirect hop.
func (g *ssrfGuard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("url_scope: split %q: %w", addr, err)
	}
	exact := g.matcher.allowsExactHost(host, port)

	var addrs []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		addrs = []netip.Addr{ip}
	} else {
		addrs, err = g.resolve(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("url_scope: resolve %s: %w", host, err)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("url_scope: resolve %s: no addresses", host)
		}
	}
	// Validate the full set before dialing anything: a host that
	// mixes a public IP with a private one is treated as hostile.
	for _, a := range addrs {
		if err := g.checkAddr(a, host, exact); err != nil {
			return nil, err
		}
	}
	d := &net.Dialer{}
	var lastErr error
	for _, a := range addrs {
		conn, derr := d.DialContext(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	return nil, lastErr
}
