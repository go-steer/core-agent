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

package memory

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Scheme constants for canonical --memory-ref URIs.
const (
	SchemeMemoryBank = "memorybank"
	SchemeSQLite     = "sqlite"
	SchemeInMemory   = "inmemory"
)

// ParsedRef holds the normalized components of a --memory-ref URI.
type ParsedRef struct {
	Raw      string
	Scheme   string
	Resource string            // Canonical resource or path (for memorybank: projects/{p}/locations/{l}/reasoningEngines/{id}).
	Project  string            // Populated for memorybank:// refs.
	Location string            // Populated for memorybank:// refs.
	EngineID string            // Populated for memorybank:// refs.
	Query    map[string]string // First value of each query parameter, if any.
}

// Opener constructs a Backend from a validated ParsedRef.
type Opener func(ctx context.Context, ref ParsedRef) (Backend, error)

var (
	openerMu sync.RWMutex
	openers  = map[string]Opener{
		SchemeInMemory: openInMemoryRef,
	}

	namedInMemoryMu sync.Mutex
	namedInMemory   = map[string]*sharedInMemoryHandle{}
)

// RegisterOpener registers (or replaces) the Backend Opener for scheme
// (for example, "memorybank", "sqlite", "redis-ams"). Panics if scheme is
// empty or opener is nil.
func RegisterOpener(scheme string, opener Opener) {
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme == "" {
		panic("memory.RegisterOpener: empty scheme")
	}
	if opener == nil {
		panic("memory.RegisterOpener: nil opener")
	}
	openerMu.Lock()
	defer openerMu.Unlock()
	openers[scheme] = opener
}

// RegisteredSchemes returns the sorted list of currently registered URI schemes.
func RegisteredSchemes() []string {
	openerMu.RLock()
	defer openerMu.RUnlock()
	out := make([]string, 0, len(openers))
	for k := range openers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// OpenRouter opens spec.Ref via Open and constructs a Router over spec.Mounts.
// Callers are responsible for closing the returned Backend on shutdown.
func OpenRouter(ctx context.Context, spec BindingSpec, opts ...RouterOption) (*Router, Backend, error) {
	parsed, err := ParseRef(spec.Ref)
	if err != nil {
		return nil, nil, err
	}
	b, err := Open(ctx, spec.Ref)
	if err != nil {
		return nil, nil, err
	}
	allOpts := append([]RouterOption{
		WithBackendName(parsed.Scheme),
		WithNamespace(spec.Namespace),
	}, opts...)
	r, err := NewRouter(b, spec.Mounts, allOpts...)
	if err != nil {
		_ = b.Close()
		return nil, nil, err
	}
	return r, b, nil
}

// Open parses rawRef and invokes the registered Opener for its scheme.
func Open(ctx context.Context, rawRef string) (Backend, error) {
	parsed, err := ParseRef(rawRef)
	if err != nil {
		return nil, err
	}
	openerMu.RLock()
	opener, ok := openers[parsed.Scheme]
	openerMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (import the backend package or call memory.RegisterOpener)", ErrUnknownScheme, parsed.Scheme)
	}
	return opener(ctx, parsed)
}

// ParseRef parses and validates a memory reference URI such as:
//   - memorybank://projects/{project}/locations/{location}/reasoningEngines/{id}
//   - memorybank://{project}/{location}/{id}
//   - sqlite:///var/lib/core-agent/memory.db
//   - inmemory://default
//   - <custom-scheme>://...
func ParseRef(raw string) (ParsedRef, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ParsedRef{}, fmt.Errorf("%w: empty URI", ErrInvalidRef)
	}
	if trimmed != raw {
		return ParsedRef{}, fmt.Errorf("%w: URI %q has surrounding whitespace", ErrInvalidRef, raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ParsedRef{}, fmt.Errorf("%w: %v", ErrInvalidRef, err)
	}
	if u.Scheme == "" {
		return ParsedRef{}, fmt.Errorf("%w: missing scheme in %q", ErrInvalidRef, raw)
	}
	if u.Fragment != "" {
		return ParsedRef{}, fmt.Errorf("%w: unexpected fragment in %q", ErrInvalidRef, raw)
	}
	scheme := strings.ToLower(u.Scheme)
	q := parseQueryFirstValues(u.Query())

	switch scheme {
	case SchemeMemoryBank:
		return parseMemoryBankRef(raw, u, q)
	case SchemeSQLite:
		return parseSQLiteRef(raw, u, q)
	case SchemeInMemory:
		name := strings.TrimPrefix(u.Host+u.Path, "/")
		if strings.Contains(name, "/") {
			return ParsedRef{}, fmt.Errorf("%w: inmemory store name %q must not contain '/'", ErrInvalidRef, name)
		}
		return ParsedRef{
			Raw:      raw,
			Scheme:   SchemeInMemory,
			Resource: name,
			Query:    q,
		}, nil
	default:
		res := u.Host + u.Path
		if res == "" {
			return ParsedRef{}, fmt.Errorf("%w: %s URI %q has empty target", ErrInvalidRef, scheme, raw)
		}
		return ParsedRef{
			Raw:      raw,
			Scheme:   scheme,
			Resource: res,
			Query:    q,
		}, nil
	}
}

func parseMemoryBankRef(raw string, u *url.URL, q map[string]string) (ParsedRef, error) {
	if u.User != nil {
		return ParsedRef{}, fmt.Errorf("%w: memorybank URI must not contain userinfo", ErrInvalidRef)
	}
	combined := strings.Trim(u.Host+u.Path, "/")
	if combined == "" {
		return ParsedRef{}, fmt.Errorf("%w: memorybank URI requires ReasoningEngine resource path", ErrInvalidRef)
	}
	proj, loc, id, err := ParseReasoningEngineResource(combined)
	if err != nil {
		return ParsedRef{}, err
	}
	return ParsedRef{
		Raw:      raw,
		Scheme:   SchemeMemoryBank,
		Resource: FormatReasoningEngineResource(proj, loc, id),
		Project:  proj,
		Location: loc,
		EngineID: id,
		Query:    q,
	}, nil
}

func parseSQLiteRef(raw string, u *url.URL, q map[string]string) (ParsedRef, error) {
	if u.User != nil {
		return ParsedRef{}, fmt.Errorf("%w: sqlite URI must not contain userinfo", ErrInvalidRef)
	}
	var path string
	switch u.Host {
	case "", "localhost":
		path = u.Path
	case ".":
		path = strings.TrimPrefix(u.Path, "/")
	default:
		// Relative form sqlite://relative/path.db
		path = u.Host + u.Path
	}
	if strings.TrimSpace(path) == "" || path == "/" {
		return ParsedRef{}, fmt.Errorf("%w: sqlite URI %q requires a file path", ErrInvalidRef, raw)
	}
	return ParsedRef{
		Raw:      raw,
		Scheme:   SchemeSQLite,
		Resource: path,
		Query:    q,
	}, nil
}

// ParseReasoningEngineResource parses either full form
// ("projects/{p}/locations/{l}/reasoningEngines/{id}") or compact 3-segment
// form ("{p}/{l}/{id}") and validates every segment.
func ParseReasoningEngineResource(s string) (project, location, id string, err error) {
	trimmed := strings.Trim(strings.TrimSpace(s), "/")
	parts := strings.Split(trimmed, "/")
	switch {
	case len(parts) == 6 && parts[0] == "projects" && parts[2] == "locations" && parts[4] == "reasoningEngines":
		project, location, id = parts[1], parts[3], parts[5]
	case len(parts) == 3 && parts[0] != "projects":
		project, location, id = parts[0], parts[1], parts[2]
	default:
		return "", "", "", fmt.Errorf("%w: %q must match projects/{project}/locations/{location}/reasoningEngines/{id} or {project}/{location}/{id}", ErrInvalidRef, s)
	}
	for _, seg := range []struct{ label, val string }{
		{"project", project},
		{"location", location},
		{"reasoningEngine id", id},
	} {
		if err := validateResourceSegment(seg.label, seg.val); err != nil {
			return "", "", "", err
		}
	}
	return project, location, id, nil
}

// FormatReasoningEngineResource formats the canonical ReasoningEngine resource
// name from validated components.
func FormatReasoningEngineResource(project, location, id string) string {
	return fmt.Sprintf("projects/%s/locations/%s/reasoningEngines/%s", project, location, id)
}

func validateResourceSegment(label, val string) error {
	if val == "" || val == "." || val == ".." {
		return fmt.Errorf("%w: invalid %s %q", ErrInvalidRef, label, val)
	}
	if err := validateScopeAtom(label, val); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRef, err)
	}
	if strings.ContainsAny(val, "/?#\\ ") {
		return fmt.Errorf("%w: %s %q contains invalid URI characters", ErrInvalidRef, label, val)
	}
	return nil
}

func parseQueryFirstValues(vals url.Values) map[string]string {
	if len(vals) == 0 {
		return nil
	}
	out := make(map[string]string, len(vals))
	for k, list := range vals {
		if len(list) > 0 {
			out[k] = list[0]
		}
	}
	return maps.Clone(out)
}

// sharedInMemoryHandle wraps a named process-scoped InMemory store so Close()
// on one consumer handle does not wipe state held by sibling routers sharing
// the same inmemory://<name> URI in tests and multi-subagent smoke runs.
type sharedInMemoryHandle struct {
	store *InMemory
}

func (h *sharedInMemoryHandle) Retrieve(ctx context.Context, scope ScopeMap, req RetrieveRequest) ([]Item, error) {
	return h.store.Retrieve(ctx, scope, req)
}

func (h *sharedInMemoryHandle) Put(ctx context.Context, scope ScopeMap, req PutRequest) ([]ConsolidationDelta, error) {
	return h.store.Put(ctx, scope, req)
}

func (h *sharedInMemoryHandle) Delete(ctx context.Context, scope ScopeMap, id string) error {
	return h.store.Delete(ctx, scope, id)
}

func (h *sharedInMemoryHandle) ConsolidateConversation(ctx context.Context, scope ScopeMap, req ConversationRequest) ([]ConsolidationDelta, error) {
	return h.store.ConsolidateConversation(ctx, scope, req)
}

func (h *sharedInMemoryHandle) Close() error {
	return nil
}

func openInMemoryRef(_ context.Context, ref ParsedRef) (Backend, error) {
	if ref.Resource == "" {
		return NewInMemory(), nil
	}
	namedInMemoryMu.Lock()
	defer namedInMemoryMu.Unlock()
	h, ok := namedInMemory[ref.Resource]
	if !ok {
		h = &sharedInMemoryHandle{store: NewInMemory()}
		namedInMemory[ref.Resource] = h
	}
	return h, nil
}
