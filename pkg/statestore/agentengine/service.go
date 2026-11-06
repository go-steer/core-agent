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

package agentengine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"github.com/google/uuid"
	"google.golang.org/adk/v2/session"
	"google.golang.org/api/iterator"
	"google.golang.org/genai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

// service is the session.Service the Transcript exposes.
type service struct {
	t *Transcript
}

func (s *service) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	if s.t.closed.Load() {
		return nil, eventlog.ErrClosed
	}
	if req == nil || req.AppName == "" || req.UserID == "" {
		return nil, errors.New("agentengine: Create: app_name and user_id are required")
	}
	id := req.SessionID
	if id == "" {
		id = uuid.NewString()
	}
	k := treeKey{req.AppName, req.UserID, eventlog.RootOf(id)}
	s.t.writeMu.Lock()
	defer s.t.writeMu.Unlock()
	tr, err := s.t.ensureTree(ctx, k)
	if err != nil {
		return nil, err
	}
	s.t.mu.Lock()
	_, exists := tr.members[id]
	s.t.mu.Unlock()
	if exists {
		return nil, fmt.Errorf("agentengine: Create: session %q already exists", id)
	}
	appOnly := withIdentity(req.State, k.app, id, k.root)
	st, err := structpb.NewStruct(appOnly)
	if err != nil {
		return nil, fmt.Errorf("agentengine: Create: encode state: %w", err)
	}
	op, err := s.t.client.CreateSession(ctx, &aiplatformpb.CreateSessionRequest{
		Parent:    s.t.parent,
		SessionId: mapID(id),
		Session: &aiplatformpb.Session{
			UserId:       req.UserID,
			DisplayName:  id,
			Labels:       map[string]string{labelRoot: mapID(k.root)},
			SessionState: st,
			Expiration:   &aiplatformpb.Session_Ttl{Ttl: durationpb.New(s.t.ttl)},
		},
	})
	if err != nil {
		return nil, wrapRPC("create session "+id, err)
	}
	created, err := op.Wait(ctx)
	if err != nil {
		return nil, wrapRPC("create session "+id, err)
	}
	r := &resident{parent: s.t.parent, appName: k.app, userID: k.user, id: id, state: appState(created.GetSessionState())}
	if len(r.state) == 0 {
		// A service that echoes no state back: keep what we sent, minus
		// our own keys, which appState would have stripped.
		r.state = appState(structMap(appOnly))
	}
	if et := created.GetExpireTime(); et != nil {
		r.expireAt = et.AsTime()
	}
	r.updatedAt = time.Now()
	if ut := created.GetUpdateTime(); ut != nil {
		r.updatedAt = ut.AsTime()
	}
	s.t.mu.Lock()
	tr.members[id] = r
	s.t.mu.Unlock()
	return &session.CreateResponse{Session: r.snapshot()}, nil
}

// withIdentity is req.State plus the keys that let the session be found
// again (identityFromState).
func withIdentity(state map[string]any, app, id, root string) map[string]any {
	out := make(map[string]any, len(state)+3)
	for k, v := range state {
		if strings.HasPrefix(k, session.KeyPrefixTemp) {
			continue
		}
		out[k] = v
	}
	out[stateKeyAppName] = app
	out[stateKeySessionID] = id
	out[stateKeyRootID] = root
	return out
}

// structMap adapts a plain map to the AsMap shape appState reads.
type structMap map[string]any

func (m structMap) AsMap() map[string]any { return m }

func (s *service) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	if s.t.closed.Load() {
		return nil, eventlog.ErrClosed
	}
	if req == nil || req.AppName == "" || req.UserID == "" || req.SessionID == "" {
		return nil, errors.New("agentengine: Get: app_name, user_id and session_id are required")
	}
	r, err := s.t.lookup(ctx, treeKey{req.AppName, req.UserID, eventlog.RootOf(req.SessionID)}, req.SessionID)
	if err != nil {
		return nil, err
	}
	snap := r.snapshot()
	snap.events = narrowEvents(snap.events, req)
	return &session.GetResponse{Session: snap}, nil
}

// narrowEvents applies GetRequest's After and NumRecentEvents.
func narrowEvents(evs localEvents, req *session.GetRequest) localEvents {
	if !req.After.IsZero() {
		kept := evs[:0:0]
		for _, ev := range evs {
			if !ev.Timestamp.Before(req.After) {
				kept = append(kept, ev)
			}
		}
		evs = kept
	}
	if req.NumRecentEvents > 0 && len(evs) > req.NumRecentEvents {
		evs = evs[len(evs)-req.NumRecentEvents:]
	}
	return evs
}

// lookup returns the resident for id, loading its tree on first touch
// and falling back to a direct GetSession for a session the tree scan
// did not surface (one created outside this backend's labelling).
func (t *Transcript) lookup(ctx context.Context, k treeKey, id string) (*resident, error) {
	tr, err := t.ensureTree(ctx, k)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	r := tr.members[id]
	t.mu.Unlock()
	if r != nil {
		return r, nil
	}
	s, err := t.client.GetSession(ctx, &aiplatformpb.GetSessionRequest{Name: t.sessionName(id)})
	if err != nil {
		return nil, wrapRPC("get session "+id, err)
	}
	// The service keys sessions by engine and ID only. A session that
	// exists under this ID but belongs to another user or app is not
	// this caller's: report it missing, as ADK's own client does, rather
	// than hand one user another's transcript — or worse, let them
	// append to it.
	if ident := identityFromState(s.GetSessionState()); s.GetUserId() != k.user || ident.app != k.app || ident.id != id {
		return nil, fmt.Errorf("agentengine: get session %q: %w", id, session.ErrNotFound)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if r = tr.members[id]; r != nil { // raced with Create
		return r, nil
	}
	r, err = t.loadResident(ctx, k, id, s)
	if err != nil {
		return nil, err
	}
	tr.members[id] = r
	if r.high() > tr.high {
		tr.high = r.high()
	}
	return r, nil
}

func (s *service) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	if s.t.closed.Load() {
		return nil, eventlog.ErrClosed
	}
	if req == nil || req.AppName == "" {
		return nil, errors.New("agentengine: List: app_name is required")
	}
	lreq := &aiplatformpb.ListSessionsRequest{Parent: s.t.parent}
	if req.UserID != "" {
		lreq.Filter = userFilter(req.UserID)
	}
	it := s.t.client.ListSessions(ctx, lreq)
	var out []session.Session
	for {
		sess, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, wrapRPC("list sessions", err)
		}
		ident := identityFromState(sess.GetSessionState())
		if ident.id == "" || ident.app != req.AppName {
			continue
		}
		ls := &localSession{parent: s.t.parent, appName: req.AppName, userID: sess.GetUserId(), id: ident.id, state: &localState{m: appState(sess.GetSessionState())}}
		if ut := sess.GetUpdateTime(); ut != nil {
			ls.updatedAt = ut.AsTime()
		}
		out = append(out, ls)
	}
	return &session.ListResponse{Sessions: out}, nil
}

func (s *service) Delete(ctx context.Context, req *session.DeleteRequest) error {
	if s.t.closed.Load() {
		return eventlog.ErrClosed
	}
	if req == nil || req.AppName == "" || req.UserID == "" || req.SessionID == "" {
		return errors.New("agentengine: Delete: app_name, user_id and session_id are required")
	}
	k := treeKey{req.AppName, req.UserID, eventlog.RootOf(req.SessionID)}
	s.t.writeMu.Lock()
	defer s.t.writeMu.Unlock()
	tr, err := s.t.ensureTree(ctx, k)
	if err != nil {
		return err
	}
	// Children first, root last: a failure midway then leaves a root
	// that still exists rather than orphans pointing at a deleted one.
	var ids []string
	if req.SessionID == k.root {
		s.t.mu.Lock()
		for id := range tr.members {
			if id != k.root {
				ids = append(ids, id)
			}
		}
		s.t.mu.Unlock()
	}
	ids = append(ids, req.SessionID)
	for _, id := range ids {
		if err := s.t.deleteOne(ctx, id); err != nil {
			return err
		}
		s.t.mu.Lock()
		delete(tr.members, id)
		s.t.mu.Unlock()
	}
	return nil
}

// deleteOne deletes a session; a missing one is a no-op (ADK's Delete
// contract).
func (t *Transcript) deleteOne(ctx context.Context, id string) error {
	op, err := t.client.DeleteSession(ctx, &aiplatformpb.DeleteSessionRequest{Name: t.sessionName(id)})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return wrapRPC("delete session "+id, err)
	}
	if err := op.Wait(ctx); err != nil && status.Code(err) != codes.NotFound {
		return wrapRPC("delete session "+id, err)
	}
	return nil
}

func (s *service) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if s.t.closed.Load() {
		return eventlog.ErrClosed
	}
	if sess == nil || ev == nil {
		return errors.New("agentengine: AppendEvent: session and event are required")
	}
	if ev.Partial {
		return nil
	}
	k := treeKey{sess.AppName(), sess.UserID(), eventlog.RootOf(sess.ID())}
	s.t.writeMu.Lock()
	defer s.t.writeMu.Unlock()
	tr, err := s.t.ensureTree(ctx, k)
	if err != nil {
		return err
	}
	s.t.mu.Lock()
	r := tr.members[sess.ID()]
	seq := tr.high + 1
	s.t.mu.Unlock()
	if r == nil {
		return fmt.Errorf("agentengine: AppendEvent: %w: %q", session.ErrNotFound, sess.ID())
	}
	stored := trimTemp(ev)
	raw, err := encodeRawEvent(seq, eventlog.EntryMetadataFrom(ctx), stored)
	if err != nil {
		return err
	}
	entry, err := decodeRawEvent(raw) // what readers will see is what was written
	if err != nil {
		return err
	}
	req := &aiplatformpb.AppendEventRequest{Name: s.t.sessionName(sess.ID()), Event: envelope(stored, raw)}
	if err := s.t.withAppendRetry(ctx, func() error {
		_, err := s.t.client.AppendEvent(ctx, req)
		return err
	}); err != nil {
		if !definiteRejection(err) {
			// The write may have landed. Forget the tree so the next seq
			// comes from the store; reusing this one would persist a
			// duplicate that a Watch cursor could never pass.
			s.t.invalidate(tr)
		}
		return wrapRPC("append event "+ev.ID, err)
	}
	s.t.mu.Lock()
	if seq > tr.high {
		tr.high = seq
	}
	s.t.mu.Unlock()
	r.apply(entry)
	if ls, ok := sess.(*localSession); ok {
		ls.append(entry.Event)
	}
	s.t.subs.Notify(k.app, k.user, k.root)
	s.t.maybeRefreshTTL(ctx, r)
	return nil
}

// definiteRejection reports whether err means the service did not
// write: a status it returns before touching storage. Anything else —
// Unavailable, DeadlineExceeded, Canceled, Internal, Unknown, a
// transport error — may have landed server-side.
func definiteRejection(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.ResourceExhausted,
		codes.NotFound, codes.PermissionDenied, codes.Unauthenticated, codes.AlreadyExists, codes.OutOfRange:
		return true
	}
	return false
}

// trimTemp returns ev without temp: state keys, which are scoped to
// the invocation and must not reach storage (ADK does the same).
func trimTemp(ev *session.Event) *session.Event {
	if len(ev.Actions.StateDelta) == 0 {
		return ev
	}
	kept := make(map[string]any, len(ev.Actions.StateDelta))
	for k, v := range ev.Actions.StateDelta {
		if !strings.HasPrefix(k, session.KeyPrefixTemp) {
			kept[k] = v
		}
	}
	if len(kept) == len(ev.Actions.StateDelta) {
		return ev
	}
	cp := *ev
	cp.Actions.StateDelta = kept
	return &cp
}

// envelope is the SessionEvent for ev: raw_event carries the record;
// the typed fields are for the console and Memory Bank.
func envelope(ev *session.Event, raw *structpb.Struct) *aiplatformpb.SessionEvent {
	ts := ev.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	// author and invocation_id are required on the wire. Events this
	// runtime synthesises outside a turn (autonomous checkpoints, for
	// one) carry no invocation; raw_event keeps the true value and the
	// envelope gets a placeholder.
	author, inv := ev.Author, ev.InvocationID
	if author == "" {
		author = syntheticAuthor
	}
	if inv == "" {
		inv = syntheticInvocation
	}
	se := &aiplatformpb.SessionEvent{
		Author:       author,
		InvocationId: inv,
		Timestamp:    timestamppb.New(ts),
		Content:      content(ev.Content),
		Actions:      actions(ev.Actions),
		ErrorCode:    ev.ErrorCode,
		ErrorMessage: ev.ErrorMessage,
		RawEvent:     raw,
		EventMetadata: &aiplatformpb.EventMetadata{
			Branch:             ev.Branch,
			TurnComplete:       ev.TurnComplete,
			Interrupted:        ev.Interrupted,
			LongRunningToolIds: ev.LongRunningToolIDs,
		},
	}
	if len(ev.CustomMetadata) > 0 {
		if cm, err := structpb.NewStruct(ev.CustomMetadata); err == nil {
			se.EventMetadata.CustomMetadata = cm
		}
	}
	return se
}

// Placeholders for the envelope's required fields on events that have
// none of their own. Never read back: raw_event is the record.
const (
	syntheticAuthor     = "core-agent"
	syntheticInvocation = "core-agent-synthetic"
)

// actions mirrors the state and artifact deltas onto the envelope so
// the service folds them into session_state (what the console shows);
// the resident folds them from raw_event regardless.
func actions(a session.EventActions) *aiplatformpb.EventActions {
	if len(a.StateDelta) == 0 && len(a.ArtifactDelta) == 0 {
		return nil
	}
	out := &aiplatformpb.EventActions{}
	if len(a.StateDelta) > 0 {
		if sd, err := structpb.NewStruct(a.StateDelta); err == nil {
			out.StateDelta = sd
		}
	}
	if len(a.ArtifactDelta) > 0 {
		out.ArtifactDelta = make(map[string]int32, len(a.ArtifactDelta))
		for k, v := range a.ArtifactDelta {
			out.ArtifactDelta[k] = int32(v) // #nosec G115 -- artifact versions are small counters
		}
	}
	return out
}

// content converts the parts Memory Bank and the console can use: text,
// function calls and function responses. Anything else is only in
// raw_event.
func content(c *genai.Content) *aiplatformpb.Content {
	if c == nil {
		return nil
	}
	out := &aiplatformpb.Content{Role: c.Role}
	for _, p := range c.Parts {
		if p == nil {
			continue
		}
		switch {
		case p.Text != "":
			out.Parts = append(out.Parts, &aiplatformpb.Part{Data: &aiplatformpb.Part_Text{Text: p.Text}})
		case p.FunctionCall != nil:
			fc := &aiplatformpb.FunctionCall{Id: p.FunctionCall.ID, Name: p.FunctionCall.Name}
			if args, err := structpb.NewStruct(p.FunctionCall.Args); err == nil {
				fc.Args = args
			}
			out.Parts = append(out.Parts, &aiplatformpb.Part{Data: &aiplatformpb.Part_FunctionCall{FunctionCall: fc}})
		case p.FunctionResponse != nil:
			fr := &aiplatformpb.FunctionResponse{Id: p.FunctionResponse.ID, Name: p.FunctionResponse.Name}
			if resp, err := structpb.NewStruct(p.FunctionResponse.Response); err == nil {
				fr.Response = resp
			}
			out.Parts = append(out.Parts, &aiplatformpb.Part{Data: &aiplatformpb.Part_FunctionResponse{FunctionResponse: fr}})
		}
	}
	if len(out.Parts) == 0 {
		return nil
	}
	return out
}

// maybeRefreshTTL extends the session's expiry when less than half the
// configured TTL remains. Best-effort: a failure here does not fail the
// append that already landed.
func (t *Transcript) maybeRefreshTTL(ctx context.Context, r *resident) {
	r.mu.RLock()
	expireAt := r.expireAt
	r.mu.RUnlock()
	if expireAt.IsZero() || time.Until(expireAt) > t.ttl/2 {
		return
	}
	updated, err := t.client.UpdateSession(ctx, &aiplatformpb.UpdateSessionRequest{
		Session: &aiplatformpb.Session{
			Name:       t.sessionName(r.id),
			Expiration: &aiplatformpb.Session_Ttl{Ttl: durationpb.New(t.ttl)},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ttl"}},
	})
	if err != nil {
		return
	}
	if et := updated.GetExpireTime(); et != nil {
		r.mu.Lock()
		r.expireAt = et.AsTime()
		r.mu.Unlock()
	}
}
