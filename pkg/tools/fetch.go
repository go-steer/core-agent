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
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/permissions"
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
			Description: "Fetch a URL via HTTP GET. Returns body, status, content-type, and final-URL after redirects. URLs must be in the operator's url_scope.allow list (typical: GitHub API, GCP APIs, internal cluster services). HTTPS by default; http:// only when explicitly allowed. Use this instead of `bash curl` so the URL + status land structured in the eventlog and the per-host header config can inject auth tokens for you. Body is capped (default 64KB) — pass max_bytes to override up to url_scope.max_body_bytes. Each redirect target is re-checked against the allowlist; a redirect to a denied host is an error, not a silent follow.",
		},
		fetchURLFunc(gate, cfg),
	)
	if err != nil {
		panic("tools: NewFetchURLTool: " + err.Error())
	}
	return t
}

// fetchURLFunc is the handler, extracted so tests can drive it
// without going through ADK's functiontool wrapper.
func fetchURLFunc(gate *permissions.Gate, cfg *config.Config) functiontool.Func[fetchURLArgs, fetchURLResult] {
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

		cap := in.MaxBytes
		if cap <= 0 || cap > scopeCap {
			cap = scopeCap
		}

		client := &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= fetchURLMaxRedirects {
					return fmt.Errorf("fetch_url: stopped after %d redirects", fetchURLMaxRedirects)
				}
				if err := matcher.check(req.URL); err != nil {
					return fmt.Errorf("fetch_url: redirect %s", err)
				}
				return nil
			},
		}

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, parsed.String(), nil)
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
