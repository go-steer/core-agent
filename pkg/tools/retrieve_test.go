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
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/pkg/digest"
)

func TestNewRetrieveRawTool_RequiresStore(t *testing.T) {
	t.Parallel()
	// Passing nil Store is a construction-time bug — the tool would
	// refuse every call, only confusing the model. Fail loudly at
	// setup instead.
	_, err := NewRetrieveRawTool(RetrieveRawOptions{})
	if err == nil || !strings.Contains(err.Error(), "Store is required") {
		t.Fatalf("expected Store-required error, got %v", err)
	}
}

func TestNewRetrieveRawTool_DefaultsNameAndDescription(t *testing.T) {
	t.Parallel()
	tl, err := NewRetrieveRawTool(RetrieveRawOptions{Store: &memStore{}})
	if err != nil {
		t.Fatalf("NewRetrieveRawTool: %v", err)
	}
	if tl.Name() != "retrieve_raw" {
		t.Errorf("default name = %q, want retrieve_raw", tl.Name())
	}
	desc := tl.Description()
	if desc == "" {
		t.Fatalf("default description should be non-empty")
	}
	// Pin the load-bearing anti-pattern clauses. Field-observed cost
	// spike on the demo (2026-07-17): Flash called retrieve_raw to
	// "double-check" a digest and re-inflated ~28k tokens, defeating
	// the wrap's savings and running the turn ~6x more expensive than
	// the same triage the day before. The description is the primary
	// nudge the model sees for this tool; softening any of these
	// clauses in a future edit reintroduces the same failure mode.
	for _, want := range []string{
		"DO NOT",
		"digest as authoritative",
		"re-inflates",
		"truncated-field marker",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description missing anti-pattern clause %q; got:\n%s", want, desc)
		}
	}
}

func TestNewRetrieveRawTool_NameAndDescriptionOverrides(t *testing.T) {
	t.Parallel()
	tl, err := NewRetrieveRawTool(RetrieveRawOptions{
		Store:       &memStore{},
		Name:        "fetch_raw",
		Description: "custom description",
	})
	if err != nil {
		t.Fatalf("NewRetrieveRawTool: %v", err)
	}
	if tl.Name() != "fetch_raw" {
		t.Errorf("name override didn't take, got %q", tl.Name())
	}
	if tl.Description() != "custom description" {
		t.Errorf("description override didn't take")
	}
}

func TestRetrieveRawFunc_HappyPath(t *testing.T) {
	t.Parallel()
	store := &memStore{}
	_ = store.Put(context.Background(), "call-1", []byte(`{"raw":"data"}`))

	res, err := retrieveRawFunc(store)(tool.Context(nil), retrieveRawArgs{CallID: "call-1"})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res.Raw != `{"raw":"data"}` {
		t.Errorf("Raw = %q, want the stored payload", res.Raw)
	}
	if res.Bytes != len(`{"raw":"data"}`) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(`{"raw":"data"}`))
	}
}

func TestRetrieveRawFunc_EmptyCallIDReturnsFriendlyError(t *testing.T) {
	t.Parallel()
	// Model-facing "error" should be a tool response, never a Go
	// error — the model needs to stay in the loop to recover.
	res, err := retrieveRawFunc(&memStore{})(tool.Context(nil), retrieveRawArgs{})
	if err != nil {
		t.Errorf("expected nil Go error (model-visible error string), got %v", err)
	}
	if !strings.HasPrefix(res.Raw, "(error:") {
		t.Errorf("Raw should carry error prefix: %q", res.Raw)
	}
	if !strings.Contains(res.Raw, "non-empty call_id") {
		t.Errorf("error message should hint at the empty call_id: %q", res.Raw)
	}
}

func TestRetrieveRawFunc_UnknownCallIDDistinguishedFromStoreFailure(t *testing.T) {
	t.Parallel()
	// Unknown call_id → error message says "no raw payload stored"
	// so the model can differentiate "typo, try another id" from
	// "store is broken, give up."
	res, err := retrieveRawFunc(&memStore{})(tool.Context(nil), retrieveRawArgs{CallID: "unknown"})
	if err != nil {
		t.Errorf("expected nil Go error, got %v", err)
	}
	if !strings.Contains(res.Raw, "no raw payload stored") {
		t.Errorf("Raw should signal unknown call_id: %q", res.Raw)
	}
	if !strings.Contains(res.Raw, `"unknown"`) {
		t.Errorf("error should quote the missing call_id: %q", res.Raw)
	}
}

func TestRetrieveRawFunc_StoreFailureSurfacesInResponse(t *testing.T) {
	t.Parallel()
	// A non-ErrNotFound Get failure should surface as a distinct
	// error message so the model can distinguish "id doesn't exist"
	// from "store is broken." Wrap the memStore to inject a failure.
	store := &memStore{getErr: errors.New("disk read error")}
	res, err := retrieveRawFunc(store)(tool.Context(nil), retrieveRawArgs{CallID: "any"})
	if err != nil {
		t.Errorf("expected nil Go error, got %v", err)
	}
	if !strings.Contains(res.Raw, "store failed") {
		t.Errorf("Raw should signal store failure: %q", res.Raw)
	}
	if !strings.Contains(res.Raw, "disk read error") {
		t.Errorf("error should include the underlying error: %q", res.Raw)
	}
}

func TestRetrieveRawFunc_LargePayloadReturnsFullSize(t *testing.T) {
	t.Parallel()
	// Bytes must reflect the RAW size, not the digest size — that's
	// the entire point of the field (model uses it to decide whether
	// to slice further before shipping downstream).
	store := &memStore{}
	big := strings.Repeat("x", 100_000)
	_ = store.Put(context.Background(), "big", []byte(big))

	res, _ := retrieveRawFunc(store)(tool.Context(nil), retrieveRawArgs{CallID: "big"})
	if res.Bytes != 100_000 {
		t.Errorf("Bytes = %d, want 100000", res.Bytes)
	}
	if len(res.Raw) != 100_000 {
		t.Errorf("Raw len = %d, want 100000", len(res.Raw))
	}
}

// memStore is a minimal in-memory Store for tests. Not thread-safe
// beyond a coarse mutex — retrieve_raw tests are sequential.
type memStore struct {
	mu     sync.Mutex
	data   map[string][]byte
	getErr error
}

func (m *memStore) Put(_ context.Context, id string, raw []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	m.data[id] = append([]byte(nil), raw...)
	return nil
}

func (m *memStore) Get(_ context.Context, id string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	v, ok := m.data[id]
	if !ok {
		return nil, digest.ErrNotFound
	}
	return v, nil
}
