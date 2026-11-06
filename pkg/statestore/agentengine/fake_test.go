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

package agentengine_test

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/go-steer/core-agent/v3/pkg/statestore/agentengine"
)

// fakeServer is an in-memory Agent Engine Sessions API: enough of
// SessionService (and the Operations service its LROs need) for the
// backend to be exercised through the real generated client over
// bufconn. It enforces what the real service enforces and the backend
// relies on: the session-ID charset, ALREADY_EXISTS on a duplicate
// create, NOT_FOUND on unknown names, server-assigned event names,
// timestamp ordering with page_size paging, and the userId filter.
type fakeServer struct {
	aiplatformpb.UnimplementedSessionServiceServer
	longrunningpb.UnimplementedOperationsServer

	mu       sync.Mutex
	sessions map[string]*aiplatformpb.Session        // by name
	events   map[string][]*aiplatformpb.SessionEvent // by session name, append order
	nextID   int
	pageSize int // 0 = 100, the service default

	// counters for assertions
	appendCalls, listSessionsCalls, listEventsCalls, getSessionCalls, updateCalls int
	// failAppend, when set, makes AppendEvent return this status code once.
	failAppend codes.Code
	// storeThenFail, when set with failAppend, stores the event and THEN
	// returns the error: the ambiguous outcome of a lost reply.
	storeThenFail bool
	// slowCreate makes CreateSession return a not-done operation whose
	// GetOperation completes it: the real service's shape.
	slowCreate bool
	pending    map[string]*aiplatformpb.Session // operation name → result
}

var fakeIDPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

func newFakeServer() *fakeServer {
	return &fakeServer{sessions: map[string]*aiplatformpb.Session{}, events: map[string][]*aiplatformpb.SessionEvent{}}
}

// start serves the fake on bufconn and returns client options for it.
func (f *fakeServer) start(t *testing.T) []option.ClientOption {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	aiplatformpb.RegisterSessionServiceServer(srv, f)
	longrunningpb.RegisterOperationsServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(); srv.Stop() })
	return []option.ClientOption{option.WithGRPCConn(conn), option.WithoutAuthentication()}
}

func doneOp(name string, msg proto.Message) (*longrunningpb.Operation, error) {
	a, err := anypb.New(msg)
	if err != nil {
		return nil, err
	}
	return &longrunningpb.Operation{Name: name, Done: true, Result: &longrunningpb.Operation_Response{Response: a}}, nil
}

func (f *fakeServer) CreateSession(_ context.Context, req *aiplatformpb.CreateSessionRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := req.GetSessionId()
	if id == "" {
		f.nextID++
		id = fmt.Sprintf("auto-%d", f.nextID)
	}
	if !fakeIDPattern.MatchString(id) {
		return nil, status.Errorf(codes.InvalidArgument, "session id %q does not match [a-z][a-z0-9-]{0,62}", id)
	}
	name := req.GetParent() + "/sessions/" + id
	if _, ok := f.sessions[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "session %q exists", name)
	}
	s := proto.Clone(req.GetSession()).(*aiplatformpb.Session)
	s.Name = name
	now := time.Now()
	s.CreateTime = timestamppb.New(now)
	s.UpdateTime = timestamppb.New(now)
	if ttl, ok := s.GetExpiration().(*aiplatformpb.Session_Ttl); ok && ttl.Ttl != nil {
		s.Expiration = &aiplatformpb.Session_ExpireTime{ExpireTime: timestamppb.New(now.Add(ttl.Ttl.AsDuration()))}
	}
	f.sessions[name] = s
	f.events[name] = nil
	if f.slowCreate {
		opName := name + "/operations/create"
		if f.pending == nil {
			f.pending = map[string]*aiplatformpb.Session{}
		}
		f.pending[opName] = s
		return &longrunningpb.Operation{Name: opName, Done: false}, nil
	}
	return doneOp(name+"/operations/create", s)
}

func (f *fakeServer) GetSession(_ context.Context, req *aiplatformpb.GetSessionRequest) (*aiplatformpb.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getSessionCalls++
	s, ok := f.sessions[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetName())
	}
	return proto.Clone(s).(*aiplatformpb.Session), nil
}

func (f *fakeServer) UpdateSession(_ context.Context, req *aiplatformpb.UpdateSessionRequest) (*aiplatformpb.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	s, ok := f.sessions[req.GetSession().GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetSession().GetName())
	}
	if ttl, ok := req.GetSession().GetExpiration().(*aiplatformpb.Session_Ttl); ok && ttl.Ttl != nil {
		s.Expiration = &aiplatformpb.Session_ExpireTime{ExpireTime: timestamppb.New(time.Now().Add(ttl.Ttl.AsDuration()))}
	}
	s.UpdateTime = timestamppb.Now()
	return proto.Clone(s).(*aiplatformpb.Session), nil
}

var userFilter = regexp.MustCompile(`^userId="((?:[^"\\]|\\.)*)"$`)

func (f *fakeServer) ListSessions(_ context.Context, req *aiplatformpb.ListSessionsRequest) (*aiplatformpb.ListSessionsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listSessionsCalls++
	var user string
	if fl := req.GetFilter(); fl != "" {
		m := userFilter.FindStringSubmatch(fl)
		if m == nil {
			return nil, status.Errorf(codes.InvalidArgument, "unsupported filter %q", fl)
		}
		user = strings.ReplaceAll(m[1], `\"`, `"`)
	}
	names := make([]string, 0, len(f.sessions))
	for name, s := range f.sessions {
		if strings.HasPrefix(name, req.GetParent()+"/sessions/") && (user == "" || s.GetUserId() == user) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	resp := &aiplatformpb.ListSessionsResponse{}
	for _, n := range names {
		resp.Sessions = append(resp.Sessions, proto.Clone(f.sessions[n]).(*aiplatformpb.Session))
	}
	return resp, nil
}

func (f *fakeServer) DeleteSession(_ context.Context, req *aiplatformpb.DeleteSessionRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[req.GetName()]; !ok {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetName())
	}
	delete(f.sessions, req.GetName())
	delete(f.events, req.GetName())
	return doneOp(req.GetName()+"/operations/delete", &aiplatformpb.DeleteOperationMetadata{})
}

func (f *fakeServer) AppendEvent(_ context.Context, req *aiplatformpb.AppendEventRequest) (*aiplatformpb.AppendEventResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendCalls++
	if f.failAppend != codes.OK && !f.storeThenFail {
		code := f.failAppend
		f.failAppend = codes.OK
		return nil, status.Errorf(code, "injected")
	}
	if _, ok := f.sessions[req.GetName()]; !ok {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetName())
	}
	ev := proto.Clone(req.GetEvent()).(*aiplatformpb.SessionEvent)
	f.nextID++
	ev.Name = req.GetName() + "/events/" + strconv.Itoa(f.nextID)
	if ev.Timestamp == nil {
		ev.Timestamp = timestamppb.Now()
	}
	f.events[req.GetName()] = append(f.events[req.GetName()], ev)
	f.sessions[req.GetName()].UpdateTime = timestamppb.Now()
	if f.failAppend != codes.OK && f.storeThenFail {
		code := f.failAppend
		f.failAppend, f.storeThenFail = codes.OK, false
		return nil, status.Errorf(code, "injected after write")
	}
	return &aiplatformpb.AppendEventResponse{}, nil
}

func (f *fakeServer) ListEvents(_ context.Context, req *aiplatformpb.ListEventsRequest) (*aiplatformpb.ListEventsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listEventsCalls++
	if _, ok := f.sessions[req.GetParent()]; !ok {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetParent())
	}
	evs := append([]*aiplatformpb.SessionEvent(nil), f.events[req.GetParent()]...)
	desc := strings.HasSuffix(strings.ToLower(req.GetOrderBy()), "desc")
	sort.SliceStable(evs, func(i, j int) bool {
		a, b := evs[i].GetTimestamp().AsTime(), evs[j].GetTimestamp().AsTime()
		if desc {
			return a.After(b)
		}
		return a.Before(b)
	})
	size := int(req.GetPageSize())
	if size <= 0 {
		size = f.pageSize
	}
	if size <= 0 {
		size = 100
	}
	start := 0
	if tok := req.GetPageToken(); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "bad page token")
		}
		start = n
	}
	end := start + size
	if end > len(evs) {
		end = len(evs)
	}
	resp := &aiplatformpb.ListEventsResponse{}
	for _, ev := range evs[start:end] {
		resp.SessionEvents = append(resp.SessionEvents, proto.Clone(ev).(*aiplatformpb.SessionEvent))
	}
	if end < len(evs) {
		resp.NextPageToken = strconv.Itoa(end)
	}
	return resp, nil
}

// GetOperation serves the Operations service for LRO polling. A
// pending create (slowCreate) completes here with its Session as the
// result, which is the path the real service takes.
func (f *fakeServer) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.pending[req.GetName()]; ok {
		delete(f.pending, req.GetName())
		return doneOp(req.GetName(), s)
	}
	return nil, status.Errorf(codes.NotFound, "operation %q not found", req.GetName())
}

const testEngine = "projects/p/locations/us-central1/reasoningEngines/123"

// openFake returns a Transcript on a fresh fake server.
func openFake(t *testing.T) (*agentengine.Transcript, *fakeServer) {
	t.Helper()
	f := newFakeServer()
	tr, err := agentengine.Open(context.Background(), agentengine.Config{
		ReasoningEngine: testEngine,
		ClientOptions:   f.start(t),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr, f
}

// reopenFake returns a second Transcript on the same fake server: the
// handover shape, where a new process finds the store populated.
func reopenFake(t *testing.T, f *fakeServer) *agentengine.Transcript {
	t.Helper()
	tr, err := agentengine.Open(context.Background(), agentengine.Config{
		ReasoningEngine: testEngine,
		ClientOptions:   f.start(t),
	})
	if err != nil {
		t.Fatalf("Open (reopen): %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}
