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

package eventlog

import (
	"context"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
	"gorm.io/gorm"
)

const (
	spendTestApp  = "core-agent"
	spendTestUser = "alice"
	spendTestSID  = "s1"
)

func openSpendStore(t *testing.T) (*SpendStore, *gorm.DB) {
	t.Helper()
	h, err := Open(context.Background(), sqlite.Open(filepath.Join(t.TempDir(), "eventlog.db")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if h.Spend == nil {
		t.Fatal("Handle.Spend is nil after Open")
	}
	return h.Spend, h.DB
}

func sameUSD(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

func TestSpendStoreFoldsBySessionAndAuthor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openSpendStore(t)

	st, err := s.Fold(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Fatalf("Fold on an empty ledger: %v", err)
	}
	if st.Session != (SpendTotals{}) || len(st.ByAuthor) != 0 || st.Unpriced != 0 {
		t.Fatalf("an empty ledger folded to %+v, want the zero value", st)
	}

	at := time.Date(2026, 8, 18, 14, 0, 0, 0, time.UTC)
	rows := []SpendRecord{
		{Author: "coordinator", Tokens: 1_200, CostUSD: 0.06, At: at},
		{Author: "cluster", Tokens: 800, CostUSD: 0.04, At: at.Add(time.Second)},
		{Author: "coordinator", Tokens: 400, CostUSD: 0.02, At: at.Add(2 * time.Second)},
		{Author: "cluster", Tokens: 600, CostUSD: 0.03, Unpriced: true, At: at.Add(3 * time.Second)},
	}
	for i, r := range rows {
		if err := s.Append(ctx, spendTestApp, spendTestUser, spendTestSID, r); err != nil {
			t.Fatalf("Append row %d: %v", i, err)
		}
	}

	st, err = s.Fold(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if st.Session.Tokens != 3_000 || st.Session.Calls != 4 || !sameUSD(st.Session.CostUSD, 0.15) {
		t.Errorf("session total = %+v, want 3000 tokens / $0.1500 / 4 calls", st.Session)
	}
	if st.Unpriced != 1 {
		t.Errorf("unpriced = %d, want 1", st.Unpriced)
	}
	coord := st.ByAuthor["coordinator"]
	if coord.Tokens != 1_600 || coord.Calls != 2 || !sameUSD(coord.CostUSD, 0.08) {
		t.Errorf("coordinator = %+v, want 1600 tokens / $0.0800 / 2 calls", coord)
	}
	spec := st.ByAuthor["cluster"]
	if spec.Tokens != 1_400 || spec.Calls != 2 || !sameUSD(spec.CostUSD, 0.07) {
		t.Errorf("cluster = %+v, want 1400 tokens / $0.0700 / 2 calls", spec)
	}
}

func TestSpendStoreFoldTreeIncludesDerivedSubSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openSpendStore(t)

	if err := s.Append(ctx, spendTestApp, spendTestUser, spendTestSID, SpendRecord{Author: "parent", Tokens: 1_000, CostUSD: 0.05}); err != nil {
		t.Fatalf("Append parent: %v", err)
	}
	if err := s.Append(ctx, spendTestApp, spendTestUser, spendTestSID+":sub:cluster:inv1", SpendRecord{Author: "cluster", Tokens: 2_000, CostUSD: 0.20}); err != nil {
		t.Fatalf("Append subsession: %v", err)
	}
	if err := s.Append(ctx, spendTestApp, spendTestUser, "other-session:sub:cluster:inv2", SpendRecord{Author: "cluster", Tokens: 9_000, CostUSD: 9.00}); err != nil {
		t.Fatalf("Append other subsession: %v", err)
	}

	st, err := s.FoldTree(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Fatalf("FoldTree: %v", err)
	}
	if st.Session.Tokens != 3_000 || st.Session.Calls != 2 || !sameUSD(st.Session.CostUSD, 0.25) {
		t.Fatalf("FoldTree session = %+v, want 3000 tokens / $0.25 / 2 calls", st.Session)
	}
}

func TestSpendStoreSurvivesAReopenAndScopesBySession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, db := openSpendStore(t)

	if err := s.Append(ctx, spendTestApp, spendTestUser, spendTestSID, SpendRecord{Author: "coordinator", Tokens: 1_000, CostUSD: 0.05}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	for _, n := range []struct{ app, user, sid string }{
		{spendTestApp, spendTestUser, "other-session"},
		{spendTestApp, "someone-else", spendTestSID},
		{"other-app", spendTestUser, spendTestSID},
	} {
		if err := s.Append(ctx, n.app, n.user, n.sid, SpendRecord{Author: "coordinator", Tokens: 9_000, CostUSD: 9.99}); err != nil {
			t.Fatalf("Append neighbour %+v: %v", n, err)
		}
	}

	reopened, err := NewSpendStore(ctx, db)
	if err != nil {
		t.Fatalf("NewSpendStore on an existing database: %v", err)
	}
	st, err := reopened.Fold(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if st.Session.Calls != 1 || st.Session.Tokens != 1_000 || !sameUSD(st.Session.CostUSD, 0.05) {
		t.Fatalf("after reopen: %+v, want the one row this session wrote (1000 tokens / $0.0500)", st.Session)
	}
}

func TestSpendStoreHistoryIsOldestFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openSpendStore(t)

	at := time.Date(2026, 8, 18, 14, 0, 0, 0, time.UTC)
	for i, tok := range []int64{100, 200, 300} {
		if err := s.Append(ctx, spendTestApp, spendTestUser, spendTestSID, SpendRecord{
			Author: "coordinator", Tokens: tok, CostUSD: float64(tok) / 20_000,
			At: at.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	hist, err := s.History(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("History returned %d rows, want 3", len(hist))
	}
	for i, want := range []int64{100, 200, 300} {
		if hist[i].Tokens != want {
			t.Errorf("row %d = %d tokens, want %d (History must be oldest-first)", i, hist[i].Tokens, want)
		}
	}
	if !hist[0].At.Equal(at) {
		t.Errorf("row 0 At = %s, want the recorded %s", hist[0].At, at)
	}
}

func TestNilSpendStoreIsInert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var s *SpendStore

	if err := s.Append(ctx, spendTestApp, spendTestUser, spendTestSID, SpendRecord{Tokens: 10, CostUSD: 1}); err != nil {
		t.Errorf("Append on a nil store: %v", err)
	}
	st, err := s.Fold(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Errorf("Fold on a nil store: %v", err)
	}
	if st.Session != (SpendTotals{}) {
		t.Errorf("Fold on a nil store returned %+v, want the zero value", st.Session)
	}
	stTree, err := s.FoldTree(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Errorf("FoldTree on a nil store: %v", err)
	}
	if stTree.Session != (SpendTotals{}) {
		t.Errorf("FoldTree on a nil store returned %+v, want the zero value", stTree.Session)
	}
	hist, err := s.History(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil || hist != nil {
		t.Errorf("History on a nil store = %v, %v; want nil, nil", hist, err)
	}
}

func TestSpendStoreAppendIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openSpendStore(t)

	const writers = 8
	const each = 5
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := s.Append(ctx, spendTestApp, spendTestUser, spendTestSID, SpendRecord{
					Author: "coordinator", Tokens: 100, CostUSD: 0.005,
				}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Append: %v", err)
	}

	st, err := s.Fold(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if st.Session.Calls != writers*each || st.Session.Tokens != int64(writers*each*100) {
		t.Errorf("folded %+v, want %d calls / %d tokens", st.Session, writers*each, writers*each*100)
	}
}

func TestSpendStoreAppendRequiresAppAndSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openSpendStore(t)

	if err := s.Append(ctx, spendTestApp, spendTestUser, "", SpendRecord{Tokens: 1}); err == nil {
		t.Error("Append with no session was accepted")
	}
	if err := s.Append(ctx, "", spendTestUser, spendTestSID, SpendRecord{Tokens: 1}); err == nil {
		t.Error("Append with no app was accepted")
	}
}

func TestStampModelVersionAndHydratorPrefetch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, err := Open(ctx, sqlite.Open(filepath.Join(t.TempDir(), "eventlog.db")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	rootSID := "root-tree"
	if _, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: spendTestApp, UserID: spendTestUser, SessionID: rootSID,
	}); err != nil {
		t.Fatalf("Create root: %v", err)
	}
	rootResp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: spendTestApp, UserID: spendTestUser, SessionID: rootSID,
	})
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	rootEv := &session.Event{
		ID:        "ev-root",
		Author:    "agent",
		Timestamp: time.Now(),
		LLMResponse: adkmodel.LLMResponse{
			ModelVersion: "claude-opus-4-6-20260601",
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "parent"}}},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 100, CandidatesTokenCount: 50, TotalTokenCount: 150,
			},
		},
	}
	if err := h.Service.AppendEvent(ctx, rootResp.Session, rootEv); err != nil {
		t.Fatalf("AppendEvent root: %v", err)
	}
	if got := rootEv.CustomMetadata["model_version"]; got != "claude-opus-4-6-20260601" {
		t.Errorf("stampModelVersion CustomMetadata[model_version] = %v, want claude-opus-4-6-20260601", got)
	}

	for _, suffix := range []string{"a", "b", "c"} {
		childSID := rootSID + ":sub:" + suffix
		if _, err := h.Service.Create(ctx, &session.CreateRequest{
			AppName: spendTestApp, UserID: spendTestUser, SessionID: childSID,
		}); err != nil {
			t.Fatalf("Create %s: %v", childSID, err)
		}
		childResp, err := h.Service.Get(ctx, &session.GetRequest{
			AppName: spendTestApp, UserID: spendTestUser, SessionID: childSID,
		})
		if err != nil {
			t.Fatalf("Get %s: %v", childSID, err)
		}
		childEv := &session.Event{
			ID:        "ev-" + suffix,
			Author:    "sub-" + suffix,
			Branch:    suffix,
			Timestamp: time.Now(),
			LLMResponse: adkmodel.LLMResponse{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: suffix}}},
			},
		}
		if err := h.Service.AppendEvent(ctx, childResp.Session, childEv); err != nil {
			t.Fatalf("AppendEvent %s: %v", childSID, err)
		}
	}

	var gotIDs []string
	var rootModelVersion string
	for ev, iterErr := range h.Stream.Since(ctx, 0, WithSessionTree(spendTestApp, spendTestUser, rootSID)) {
		if iterErr != nil {
			t.Fatalf("Since WithSessionTree: %v", iterErr)
		}
		gotIDs = append(gotIDs, ev.Event.ID)
		if ev.Event.ID == "ev-root" {
			rootModelVersion = ev.Event.ModelVersion
		}
	}
	if len(gotIDs) != 4 {
		t.Fatalf("WithSessionTree returned %v, want 4 events across root and 3 child sessions", gotIDs)
	}
	if rootModelVersion != "claude-opus-4-6-20260601" {
		t.Errorf("rehydrated ev-root ModelVersion = %q, want claude-opus-4-6-20260601", rootModelVersion)
	}
}

func TestServiceDelete_CleansUpRootAndSubsessionTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, err := Open(ctx, sqlite.Open(filepath.Join(t.TempDir(), "eventlog.db")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	rootSID := "s-del"
	childSID := rootSID + ":sub:research:inv1"
	emptyChildSID := rootSID + ":sub:empty:inv2"
	keepSID := "s-del-other"
	if _, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: spendTestApp, UserID: spendTestUser, SessionID: emptyChildSID,
	}); err != nil {
		t.Fatalf("Create %s: %v", emptyChildSID, err)
	}
	for _, sid := range []string{rootSID, childSID, keepSID} {
		if _, err := h.Service.Create(ctx, &session.CreateRequest{
			AppName: spendTestApp, UserID: spendTestUser, SessionID: sid,
		}); err != nil {
			t.Fatalf("Create %s: %v", sid, err)
		}
		resp, err := h.Service.Get(ctx, &session.GetRequest{
			AppName: spendTestApp, UserID: spendTestUser, SessionID: sid,
		})
		if err != nil {
			t.Fatalf("Get %s: %v", sid, err)
		}
		ev := &session.Event{
			ID:        "ev-" + sid,
			Author:    "agent",
			Timestamp: time.Now(),
			LLMResponse: adkmodel.LLMResponse{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}},
			},
		}
		if err := h.Service.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatalf("AppendEvent %s: %v", sid, err)
		}
		if err := h.Spend.Append(ctx, spendTestApp, spendTestUser, sid, SpendRecord{Author: "agent", Tokens: 100, CostUSD: 0.01}); err != nil {
			t.Fatalf("Spend.Append %s: %v", sid, err)
		}
		if err := h.Guardrail.TripWatchdog(ctx, spendTestApp, spendTestUser, sid, "timeout"); err != nil {
			t.Fatalf("Guardrail.TripWatchdog %s: %v", sid, err)
		}
	}

	if err := h.Service.Delete(ctx, &session.DeleteRequest{
		AppName: spendTestApp, UserID: spendTestUser, SessionID: rootSID,
	}); err != nil {
		t.Fatalf("Delete root: %v", err)
	}

	stTree, err := h.Spend.FoldTree(ctx, spendTestApp, spendTestUser, rootSID)
	if err != nil {
		t.Fatalf("FoldTree after delete: %v", err)
	}
	if stTree.Session.Calls != 0 {
		t.Errorf("FoldTree after delete = %+v, want 0 calls (child spend must be deleted)", stTree.Session)
	}
	childGuardrail, err := h.Guardrail.Load(ctx, spendTestApp, spendTestUser, childSID)
	if err != nil {
		t.Fatalf("Load child: %v", err)
	}
	if childGuardrail.WatchdogTripped {
		t.Errorf("child GuardrailState after root delete = %+v, want WatchdogTripped=false", childGuardrail)
	}
	if _, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: spendTestApp, UserID: spendTestUser, SessionID: childSID,
	}); err == nil {
		t.Error("expected child session in ADK sessions table to be deleted")
	}
	if _, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: spendTestApp, UserID: spendTestUser, SessionID: emptyChildSID,
	}); err == nil {
		t.Error("expected event-less child session in ADK sessions table to be deleted")
	}
	var leftoverADKEvents int64
	if err := h.DB.WithContext(ctx).Table(adkEventsTable).
		Where("app_name = ? AND user_id = ? AND (session_id = ? OR session_id LIKE ?)",
			spendTestApp, spendTestUser, rootSID, rootSID+":sub:%").
		Count(&leftoverADKEvents).Error; err != nil {
		t.Fatalf("count ADK events: %v", err)
	}
	if leftoverADKEvents != 0 {
		t.Errorf("leftover ADK events for deleted root/subsession tree = %d, want 0", leftoverADKEvents)
	}
	// Sibling session with a shared prefix ("s-del-other") must remain intact.
	stKeep, err := h.Spend.Fold(ctx, spendTestApp, spendTestUser, keepSID)
	if err != nil {
		t.Fatalf("Fold keepSID: %v", err)
	}
	if stKeep.Session.Calls != 1 {
		t.Errorf("keepSID spend calls = %d, want 1", stKeep.Session.Calls)
	}
}
