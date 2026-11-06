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

// Package agentengine is the eventlog.Transcript backend on Vertex AI
// Agent Engine Sessions, written on aiplatform/apiv1beta1.SessionClient
// directly (docs/external-state-design.md decision 7: ADK's own
// session/vertexai client lets the service assign event IDs and drops
// UsageMetadata and FinishReason on ordinary events, which resume and
// the usage tracker read back).
//
// What it stores, per decision 6: every SessionEvent carries the whole
// ADK event as JSON in raw_event, together with the seq this process
// assigned and the caller sidecar. The envelope fields (author, content
// as text parts, timestamp, branch, custom metadata) are populated too,
// for the console and Memory Bank, but on read raw_event is the record.
//
// Session IDs are mapped where Agent Engine's charset requires it
// (decision 8; see mapID), and the original ID is kept in session
// state. One process is the writer for a session (decision 1), so once
// a session is loaded the in-memory copy is authoritative: Get and
// Since read it and cost no RPC. A tree is loaded whole on first touch
// (one ListSessions plus one ListEvents per member) so the next seq is
// known across every session in it.
package agentengine

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1beta1"
	"cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/adk/v2/session"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

// DefaultSessionTTL is the session expiry written when Config.SessionTTL
// is zero: 30 days. Agent Engine's minimum is 24 hours.
const DefaultSessionTTL = 720 * time.Hour

// Config selects the reasoning engine that holds the sessions.
type Config struct {
	// ReasoningEngine is the full resource name,
	// projects/P/locations/L/reasoningEngines/N. The project and
	// location are read from it; the regional API endpoint
	// (L-aiplatform.googleapis.com) is derived from the location
	// unless ClientOptions carries its own option.WithEndpoint.
	ReasoningEngine string
	// SessionTTL is the expiry set on every session at creation and
	// refreshed on append once less than half of it remains. Zero
	// means DefaultSessionTTL.
	SessionTTL time.Duration
	// ClientOptions are passed to aiplatform.NewSessionClient after the
	// endpoint, so a caller's option wins. Tests use WithGRPCConn.
	ClientOptions []option.ClientOption
	// AppendRetryBackoff is the wait before the one retry a quota
	// rejection (ResourceExhausted) on AppendEvent gets, drawn from a
	// shared budget as the model-call retry does (#935). Zero means 2s;
	// negative disables the retry.
	AppendRetryBackoff time.Duration
}

// Validate checks the config without opening a connection.
func (c Config) Validate() error {
	if _, err := parseReasoningEngine(c.ReasoningEngine); err != nil {
		return err
	}
	if c.SessionTTL < 0 {
		return errors.New("agentengine: session_ttl must not be negative")
	}
	if c.SessionTTL > 0 && c.SessionTTL < 24*time.Hour {
		return fmt.Errorf("agentengine: session_ttl %s is below Agent Engine's 24h minimum", c.SessionTTL)
	}
	return nil
}

// parseReasoningEngine returns the location from a
// projects/P/locations/L/reasoningEngines/N resource name, which is
// what the regional endpoint needs; the whole name is the parent of
// every request.
func parseReasoningEngine(name string) (location string, err error) {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "reasoningEngines" ||
		parts[1] == "" || parts[3] == "" || parts[5] == "" {
		return "", fmt.Errorf("agentengine: reasoning_engine %q is not projects/P/locations/L/reasoningEngines/N", name)
	}
	return parts[3], nil
}

// Transcript is the Agent Engine backend. Construct with Open.
type Transcript struct {
	client *aiplatform.SessionClient
	parent string // the reasoning engine resource name
	ttl    time.Duration

	retryBackoff time.Duration
	retryBudget  *retryBudget

	// mu guards trees and their membership; each resident has its own
	// lock for its events and state.
	mu    sync.Mutex
	trees map[treeKey]*tree
	// writeMu serialises Create, AppendEvent and Delete so seq
	// assignment and the RPC that persists it cannot interleave
	// between two writers of one process (a parent and its subagents).
	writeMu sync.Mutex
	subs    eventlog.Subscribers
	closed  atomic.Bool
	svc     *service
}

type treeKey struct{ app, user, root string }

// tree is one root session tree as this process knows it.
type tree struct {
	loaded  bool
	members map[string]*resident // keyed by our session ID
	high    int64                // last seq assigned or seen in the tree
}

// Open connects to Agent Engine and returns the backend. No RPC is
// made until a session is touched.
func Open(ctx context.Context, cfg Config) (*Transcript, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	location, _ := parseReasoningEngine(cfg.ReasoningEngine)
	opts := append([]option.ClientOption{option.WithEndpoint(location + "-aiplatform.googleapis.com:443")}, cfg.ClientOptions...)
	client, err := aiplatform.NewSessionClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("agentengine: connect: %w", err)
	}
	ttl := cfg.SessionTTL
	if ttl == 0 {
		ttl = DefaultSessionTTL
	}
	backoff := cfg.AppendRetryBackoff
	if backoff == 0 {
		backoff = defaultAppendRetryBackoff
	}
	t := &Transcript{
		client: client, parent: cfg.ReasoningEngine, ttl: ttl, trees: make(map[treeKey]*tree),
		retryBackoff: backoff, retryBudget: newRetryBudget(),
	}
	t.svc = &service{t: t}
	return t, nil
}

// Service implements eventlog.Transcript.
func (t *Transcript) Service() session.Service { return t.svc }

// Since implements eventlog.Transcript. Reads are served from the
// loaded tree; the first call for a tree loads it.
func (t *Transcript) Since(ctx context.Context, fromSeq int64, opts ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	q := eventlog.ResolveQuery(opts...)
	return func(yield func(eventlog.Entry, error) bool) {
		if t.closed.Load() {
			yield(eventlog.Entry{}, eventlog.ErrClosed)
			return
		}
		app, user, root, ok := q.Scope()
		if !ok {
			yield(eventlog.Entry{}, fmt.Errorf("agentengine: Since: %w", eventlog.ErrUnscopedQuery))
			return
		}
		tr, err := t.ensureTree(ctx, treeKey{app, user, root})
		if err != nil {
			yield(eventlog.Entry{}, err)
			return
		}
		n := 0
		for _, e := range t.entriesSince(tr, fromSeq, app, user, q) {
			if ctx.Err() != nil {
				return
			}
			if !yield(e, nil) {
				return
			}
			n++
			if q.Limit > 0 && n >= q.Limit {
				return
			}
		}
	}
}

// entriesSince merges the members' entries into seq order, filtered.
// The member set is snapshotted under t.mu (Create and Delete mutate
// it; a Watch reader on the root tree races a subagent's Create
// otherwise), then each resident is read under its own lock.
func (t *Transcript) entriesSince(tr *tree, fromSeq int64, app, user string, q eventlog.Query) []eventlog.Entry {
	type member struct {
		id string
		r  *resident
	}
	t.mu.Lock()
	members := make([]member, 0, len(tr.members))
	for id, r := range tr.members {
		members = append(members, member{id, r})
	}
	t.mu.Unlock()
	var out []eventlog.Entry
	for _, m := range members {
		m.r.mu.RLock()
		for _, e := range m.r.entries {
			if e.Seq > fromSeq && q.Matches(app, user, m.id, e.Event) {
				out = append(out, e)
			}
		}
		m.r.mu.RUnlock()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// HighestSeq implements eventlog.Transcript: the highest seq among the
// tree's current events, 0 when it has none.
func (t *Transcript) HighestSeq(ctx context.Context, appName, userID, root string) (int64, error) {
	if t.closed.Load() {
		return 0, eventlog.ErrClosed
	}
	tr, err := t.ensureTree(ctx, treeKey{appName, userID, root})
	if err != nil {
		return 0, err
	}
	var high int64
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range tr.members {
		r.mu.RLock()
		if n := len(r.entries); n > 0 && r.entries[n-1].Seq > high {
			high = r.entries[n-1].Seq
		}
		r.mu.RUnlock()
	}
	return high, nil
}

// Subscribe implements eventlog.Transcript.
func (t *Transcript) Subscribe(appName, userID, root string) (<-chan struct{}, func()) {
	return t.subs.Subscribe(appName, userID, root)
}

// Close implements eventlog.Transcript.
func (t *Transcript) Close() error {
	if t.closed.Swap(true) {
		return nil
	}
	return t.client.Close()
}

// ensureTree returns the tree, loading it from Agent Engine on first
// touch. The load is one ListSessions for the user plus one ListEvents
// per member, under t.mu so two first-touchers do not both load. A
// slow load therefore holds up every other tree's readers for its
// duration; loads happen once per tree per process, so this is
// accepted rather than paying for per-tree locking.
func (t *Transcript) ensureTree(ctx context.Context, k treeKey) (*tree, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr := t.trees[k]
	if tr == nil {
		tr = &tree{members: make(map[string]*resident)}
		t.trees[k] = tr
	}
	if tr.loaded {
		return tr, nil
	}
	// A reload after invalidate (an append whose outcome was unknown)
	// starts from the store's view, not from the stale residents.
	tr.members = make(map[string]*resident)
	tr.high = 0
	if err := t.loadTree(ctx, k, tr); err != nil {
		return nil, err
	}
	tr.loaded = true
	return tr, nil
}

// Invalidate implements eventlog.TranscriptInvalidator: every tree under
// root (root itself and roots derived from it, "<root>:digest" and the
// like) is reloaded from the store on its next touch. A lease-holding
// Handle calls it when it takes a session's lease, because another
// instance may have appended while this one did not hold it; serving
// the old cache would resume on a truncated history and reuse a seq.
func (t *Transcript) Invalidate(appName, userID, root string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, tr := range t.trees {
		if k.app == appName && k.user == userID && (k.root == root || strings.HasPrefix(k.root, root+":")) {
			tr.loaded = false
		}
	}
}

// invalidate forgets what this process knows about a tree so the next
// touch reloads it from the store. Used when an AppendEvent RPC failed
// in a way that may still have written (F3 in the review of #1192
// phase 3): the seq this process would assign next must come from the
// store, or a duplicate seq would persist.
func (t *Transcript) invalidate(tr *tree) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr.loaded = false
}

// loadTree populates tr from the store. Caller holds t.mu.
func (t *Transcript) loadTree(ctx context.Context, k treeKey, tr *tree) error {
	it := t.client.ListSessions(ctx, &aiplatformpb.ListSessionsRequest{
		Parent: t.parent,
		Filter: userFilter(k.user),
	})
	for {
		s, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return wrapRPC("list sessions", err)
		}
		ident := identityFromState(s.GetSessionState())
		if ident.id == "" || ident.root != k.root || ident.app != k.app || s.GetUserId() != k.user {
			continue
		}
		id := ident.id
		if _, dup := tr.members[id]; dup {
			continue
		}
		r, err := t.loadResident(ctx, k, id, s)
		if err != nil {
			return err
		}
		tr.members[id] = r
		if r.high() > tr.high {
			tr.high = r.high()
		}
	}
}

// loadResident builds a resident from a Session resource and its
// events. Events this backend did not write are skipped.
func (t *Transcript) loadResident(ctx context.Context, k treeKey, id string, s *aiplatformpb.Session) (*resident, error) {
	r := &resident{parent: t.parent, appName: k.app, userID: k.user, id: id, state: appState(s.GetSessionState())}
	if et := s.GetExpireTime(); et != nil {
		r.expireAt = et.AsTime()
	}
	it := t.client.ListEvents(ctx, &aiplatformpb.ListEventsRequest{Parent: s.GetName(), OrderBy: "timestamp"})
	var entries []eventlog.Entry
	for {
		ev, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, wrapRPC("list events for "+id, err)
		}
		e, err := decodeRawEvent(ev.GetRawEvent())
		if errors.Is(err, errNotOurs) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	// Our seq is the order; the service's timestamp ordering only
	// narrowed the fetch.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Seq < entries[j].Seq })
	for _, e := range entries {
		r.apply(e)
	}
	return r, nil
}

// identity is what this backend wrote into session state to find the
// session again: our app name, session ID and root ID. Agent Engine
// keys sessions by engine and ID only, so these — with the service's
// own user_id — are what scope a session to one (app, user).
type identity struct{ app, id, root string }

// identityFromState reads the identity back out of session state.
func identityFromState(st interface{ AsMap() map[string]any }) identity {
	if st == nil {
		return identity{}
	}
	m := st.AsMap()
	var ident identity
	ident.app, _ = m[stateKeyAppName].(string)
	ident.id, _ = m[stateKeySessionID].(string)
	ident.root, _ = m[stateKeyRootID].(string)
	return ident
}

// userFilter is the ListSessions filter for one user. The literal is
// quoted the way ADK's client does it (backslash first, then quote) so
// a user ID cannot close the string early.
func userFilter(user string) string {
	esc := strings.ReplaceAll(user, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	return `userId="` + esc + `"`
}

// appState is the session state without this backend's own keys.
func appState(st interface{ AsMap() map[string]any }) map[string]any {
	out := make(map[string]any)
	if st == nil {
		return out
	}
	for k, v := range st.AsMap() {
		if strings.HasPrefix(k, "core_agent.") {
			continue
		}
		out[k] = v
	}
	return out
}

// sessionName is the resource name of one of our sessions.
func (t *Transcript) sessionName(id string) string {
	return t.parent + "/sessions/" + mapID(id)
}

// wrapRPC turns a gRPC status into the error this package reports:
// NotFound wraps session.ErrNotFound so IsSessionNotFound classifies
// it; everything else keeps its status so a caller can tell a quota
// (ResourceExhausted) or availability failure from a bad request.
func wrapRPC(op string, err error) error {
	if status.Code(err) == codes.NotFound {
		return fmt.Errorf("agentengine: %s: %w: %w", op, session.ErrNotFound, err)
	}
	return fmt.Errorf("agentengine: %s: %w", op, err)
}
