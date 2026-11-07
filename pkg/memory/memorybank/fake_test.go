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

package memorybank_test

import (
	"context"
	"fmt"
	"maps"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

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
)

const testEngine = "projects/test-proj/locations/us-central1/reasoningEngines/engine-42"

// fakeMemoryBankServer is an in-memory implementation of Vertex AI
// MemoryBankServiceServer + OperationsServer served over bufconn. It enforces
// exact ScopeMap equality, the 5-item DirectMemories ceiling on
// GenerateMemories, and LRO wrapping.
type fakeMemoryBankServer struct {
	aiplatformpb.UnimplementedMemoryBankServiceServer
	longrunningpb.UnimplementedOperationsServer

	mu       sync.Mutex
	nextID   int
	memories map[string]*aiplatformpb.Memory // full resource name -> Memory
	order    []string                        // insertion order of resource names

	// Counters and last-seen requests for assertions.
	createCalls           int
	getCalls              int
	updateCalls           int
	deleteCalls           int
	retrieveCalls         int
	generateCalls         int
	lastVertexSession     string
	lastDirectEventsCount int

	// Fault injection.
	failRetrieve  codes.Code
	failRetrieveN int
	failGenerate  codes.Code
	failGenerateN int
}

func newFakeMemoryBankServer() *fakeMemoryBankServer {
	return &fakeMemoryBankServer{
		memories: make(map[string]*aiplatformpb.Memory),
	}
}

func (f *fakeMemoryBankServer) start(t *testing.T) []option.ClientOption {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	aiplatformpb.RegisterMemoryBankServiceServer(srv, f)
	longrunningpb.RegisterOperationsServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
	})
	return []option.ClientOption{option.WithGRPCConn(conn), option.WithoutAuthentication()}
}

func doneOp(name string, msg proto.Message) (*longrunningpb.Operation, error) {
	a, err := anypb.New(msg)
	if err != nil {
		return nil, err
	}
	return &longrunningpb.Operation{
		Name:   name,
		Done:   true,
		Result: &longrunningpb.Operation_Response{Response: a},
	}, nil
}

func (f *fakeMemoryBankServer) CreateMemory(_ context.Context, req *aiplatformpb.CreateMemoryRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++

	if req.GetParent() == "" || req.GetMemory() == nil {
		return nil, status.Error(codes.InvalidArgument, "parent and memory are required")
	}
	if len(req.GetMemory().GetScope()) == 0 || len(req.GetMemory().GetScope()) > 5 {
		return nil, status.Error(codes.InvalidArgument, "scope must have 1..5 entries")
	}

	f.nextID++
	id := req.GetMemoryId()
	if id == "" {
		id = fmt.Sprintf("mem-%d", f.nextID)
	}
	name := req.GetParent() + "/memories/" + id
	now := time.Now().UTC()
	stored := proto.Clone(req.GetMemory()).(*aiplatformpb.Memory)
	stored.Name = name
	stored.CreateTime = timestamppb.New(now)
	stored.UpdateTime = timestamppb.New(now)
	if ttl, ok := stored.GetExpiration().(*aiplatformpb.Memory_Ttl); ok && ttl.Ttl != nil {
		stored.Expiration = &aiplatformpb.Memory_ExpireTime{
			ExpireTime: timestamppb.New(now.Add(ttl.Ttl.AsDuration())),
		}
	}
	f.memories[name] = stored
	f.order = append(f.order, name)
	return doneOp(name+"/operations/create", proto.Clone(stored))
}

func (f *fakeMemoryBankServer) GetMemory(_ context.Context, req *aiplatformpb.GetMemoryRequest) (*aiplatformpb.Memory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++

	m, ok := f.memories[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "memory %q not found", req.GetName())
	}
	return proto.Clone(m).(*aiplatformpb.Memory), nil
}

func (f *fakeMemoryBankServer) UpdateMemory(_ context.Context, req *aiplatformpb.UpdateMemoryRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++

	name := req.GetMemory().GetName()
	existing, ok := f.memories[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "memory %q not found", name)
	}
	now := time.Now().UTC()
	for _, p := range req.GetUpdateMask().GetPaths() {
		switch p {
		case "description":
			existing.Description = req.GetMemory().GetDescription()
		case "fact":
			existing.Fact = req.GetMemory().GetFact()
		case "ttl", "expire_time":
			if ttl, ok := req.GetMemory().GetExpiration().(*aiplatformpb.Memory_Ttl); ok && ttl.Ttl != nil {
				existing.Expiration = &aiplatformpb.Memory_ExpireTime{
					ExpireTime: timestamppb.New(now.Add(ttl.Ttl.AsDuration())),
				}
			} else if exp := req.GetMemory().GetExpireTime(); exp != nil {
				existing.Expiration = &aiplatformpb.Memory_ExpireTime{ExpireTime: exp}
			}
		}
	}
	existing.UpdateTime = timestamppb.New(now)
	return doneOp(name+"/operations/update", proto.Clone(existing))
}

func (f *fakeMemoryBankServer) DeleteMemory(_ context.Context, req *aiplatformpb.DeleteMemoryRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++

	name := req.GetName()
	if _, ok := f.memories[name]; !ok {
		return nil, status.Errorf(codes.NotFound, "memory %q not found", name)
	}
	delete(f.memories, name)
	for i, n := range f.order {
		if n == name {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
	return doneOp(name+"/operations/delete", &aiplatformpb.DeleteOperationMetadata{})
}

func (f *fakeMemoryBankServer) RetrieveMemories(_ context.Context, req *aiplatformpb.RetrieveMemoriesRequest) (*aiplatformpb.RetrieveMemoriesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retrieveCalls++

	if f.failRetrieve != codes.OK {
		code := f.failRetrieve
		if f.failRetrieveN > 1 {
			f.failRetrieveN--
		} else {
			f.failRetrieve, f.failRetrieveN = codes.OK, 0
		}
		return nil, status.Errorf(code, "injected retrieve error")
	}

	prefix := req.GetParent() + "/memories/"
	wantScope := req.GetScope()
	sim := req.GetSimilaritySearchParams()
	simple := req.GetSimpleRetrievalParams()

	var query string
	topK := 10
	if sim != nil {
		query = strings.TrimSpace(sim.GetSearchQuery())
		if sim.GetTopK() > 0 {
			topK = int(sim.GetTopK())
		}
	} else if simple != nil && simple.GetPageSize() > 0 {
		topK = int(simple.GetPageSize())
	}

	queryTokens := fakeTokenize(query)
	var hits []*aiplatformpb.RetrieveMemoriesResponse_RetrievedMemory
	for _, name := range f.order {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		m := f.memories[name]
		if m == nil || !maps.Equal(m.GetScope(), wantScope) {
			continue
		}
		rm := &aiplatformpb.RetrieveMemoriesResponse_RetrievedMemory{
			Memory: proto.Clone(m).(*aiplatformpb.Memory),
		}
		if query != "" {
			dist, matched := fakeDistance(query, queryTokens, m.GetFact())
			if !matched {
				continue
			}
			rm.Distance = dist
		}
		hits = append(hits, rm)
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if query != "" && hits[i].GetDistance() != hits[j].GetDistance() {
			return hits[i].GetDistance() < hits[j].GetDistance()
		}
		return hits[i].GetMemory().GetUpdateTime().AsTime().After(hits[j].GetMemory().GetUpdateTime().AsTime())
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return &aiplatformpb.RetrieveMemoriesResponse{RetrievedMemories: hits}, nil
}

func (f *fakeMemoryBankServer) GenerateMemories(_ context.Context, req *aiplatformpb.GenerateMemoriesRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.generateCalls++

	if f.failGenerate != codes.OK {
		code := f.failGenerate
		if f.failGenerateN > 1 {
			f.failGenerateN--
		} else {
			f.failGenerate, f.failGenerateN = codes.OK, 0
		}
		return nil, status.Errorf(code, "injected generate error")
	}

	scope := req.GetScope()
	if len(scope) == 0 || len(scope) > 5 {
		return nil, status.Error(codes.InvalidArgument, "scope must have 1..5 entries")
	}

	var candidateFacts []string
	switch src := req.GetSource().(type) {
	case *aiplatformpb.GenerateMemoriesRequest_DirectMemoriesSource_:
		dms := src.DirectMemoriesSource.GetDirectMemories()
		if len(dms) == 0 || len(dms) > 5 {
			return nil, status.Errorf(codes.InvalidArgument, "DirectMemories count %d must be in 1..5", len(dms))
		}
		for _, dm := range dms {
			candidateFacts = append(candidateFacts, strings.TrimSpace(dm.GetFact()))
		}
	case *aiplatformpb.GenerateMemoriesRequest_DirectContentsSource_:
		evs := src.DirectContentsSource.GetEvents()
		f.lastDirectEventsCount = len(evs)
		for _, ev := range evs {
			for _, p := range ev.GetContent().GetParts() {
				if txt := strings.TrimSpace(p.GetText()); txt != "" {
					candidateFacts = append(candidateFacts, fmt.Sprintf("[%s] %s", ev.GetContent().GetRole(), txt))
				}
			}
		}
	case *aiplatformpb.GenerateMemoriesRequest_VertexSessionSource_:
		sess := src.VertexSessionSource.GetSession()
		f.lastVertexSession = sess
		candidateFacts = append(candidateFacts, "Consolidated from Vertex session "+sess)
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported GenerateMemories source")
	}

	prefix := req.GetParent() + "/memories/"
	var generated []*aiplatformpb.GenerateMemoriesResponse_GeneratedMemory
	now := time.Now().UTC()

	for _, factText := range candidateFacts {
		if factText == "" {
			continue
		}
		norm := strings.ToLower(strings.Join(strings.Fields(factText), " "))
		var existing *aiplatformpb.Memory
		for _, name := range f.order {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			m := f.memories[name]
			if m != nil && maps.Equal(m.GetScope(), scope) {
				if strings.ToLower(strings.Join(strings.Fields(m.GetFact()), " ")) == norm {
					existing = m
					break
				}
			}
		}
		if existing != nil {
			existing.Fact = factText
			existing.UpdateTime = timestamppb.New(now)
			generated = append(generated, &aiplatformpb.GenerateMemoriesResponse_GeneratedMemory{
				Memory: proto.Clone(existing).(*aiplatformpb.Memory),
				Action: aiplatformpb.GenerateMemoriesResponse_GeneratedMemory_UPDATED,
			})
			continue
		}
		f.nextID++
		name := fmt.Sprintf("%smem-%d", prefix, f.nextID)
		created := &aiplatformpb.Memory{
			Name:       name,
			Fact:       factText,
			Scope:      maps.Clone(scope),
			CreateTime: timestamppb.New(now),
			UpdateTime: timestamppb.New(now),
		}
		f.memories[name] = created
		f.order = append(f.order, name)
		generated = append(generated, &aiplatformpb.GenerateMemoriesResponse_GeneratedMemory{
			Memory: proto.Clone(created).(*aiplatformpb.Memory),
			Action: aiplatformpb.GenerateMemoriesResponse_GeneratedMemory_CREATED,
		})
	}

	return doneOp(req.GetParent()+"/operations/generate", &aiplatformpb.GenerateMemoriesResponse{
		GeneratedMemories: generated,
	})
}

func fakeTokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

func fakeDistance(query string, queryTokens []string, fact string) (float64, bool) {
	lowerFact := strings.ToLower(fact)
	lowerQuery := strings.ToLower(strings.TrimSpace(query))
	if strings.Contains(lowerFact, lowerQuery) {
		return 0.1, true
	}
	if len(queryTokens) == 0 {
		return 1.0, false
	}
	factTokens := fakeTokenize(fact)
	set := make(map[string]bool, len(factTokens))
	for _, ft := range factTokens {
		set[ft] = true
	}
	hits := 0
	for _, qt := range queryTokens {
		if set[qt] {
			hits++
		}
	}
	if hits == 0 {
		return 1.0, false
	}
	return 1.0 - 0.8*(float64(hits)/float64(len(queryTokens))), true
}
