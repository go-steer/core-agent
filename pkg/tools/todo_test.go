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
	"strings"
	"testing"

	"google.golang.org/adk/tool"
)

func TestTodo_AddListSetClear(t *testing.T) {
	t.Parallel()
	store := NewTodoStore()
	fn := todoFunc(store)

	// Empty list
	res, err := fn(tool.Context(nil), todoArgs{Action: "list"})
	if err != nil || len(res.Items) != 0 {
		t.Fatalf("empty list: %v %+v", err, res)
	}

	// Add two
	if _, err := fn(tool.Context(nil), todoArgs{Action: "add", Text: "first"}); err != nil {
		t.Fatal(err)
	}
	res, _ = fn(tool.Context(nil), todoArgs{Action: "add", Text: "second"})
	if len(res.Items) != 2 || res.Items[1].ID != 2 {
		t.Fatalf("after add: %+v", res)
	}

	// Set status
	res, err = fn(tool.Context(nil), todoArgs{Action: "set_status", ID: 1, Status: "completed"})
	if err != nil || res.Items[0].Status != "completed" {
		t.Fatalf("set_status: %v %+v", err, res)
	}

	// Bad status
	_, err = fn(tool.Context(nil), todoArgs{Action: "set_status", ID: 1, Status: "wat"})
	if err == nil || !strings.Contains(err.Error(), "pending|in_progress|completed") {
		t.Errorf("expected invalid-status error, got %v", err)
	}

	// Clear
	res, _ = fn(tool.Context(nil), todoArgs{Action: "clear"})
	if len(res.Items) != 0 {
		t.Errorf("clear left items: %+v", res.Items)
	}
}

func TestTodoStore_Items_DefensiveCopy(t *testing.T) {
	t.Parallel()
	store := NewTodoStore()
	fn := todoFunc(store)
	if _, err := fn(tool.Context(nil), todoArgs{Action: "add", Text: "alpha"}); err != nil {
		t.Fatal(err)
	}
	items := store.Items()
	items[0].Text = "mutated externally"
	again := store.Items()
	if again[0].Text != "alpha" {
		t.Errorf("Items() should return a defensive copy; store leaked: %+v", again)
	}
}
