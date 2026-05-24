// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// schemaJSON is a tiny helper that builds a json.RawMessage from a Go
// literal so test cases stay readable.
func schemaJSON(t *testing.T, v map[string]any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(b)
}

func TestNewElicitState_NilRequest(t *testing.T) {
	t.Parallel()
	if _, err := newElicitState("x", nil, nil); err == nil {
		t.Errorf("expected error for nil request")
	}
}

func TestNewElicitState_URLMode(t *testing.T) {
	t.Parallel()
	out := make(chan *mcpsdk.ElicitResult, 1)
	st, err := newElicitState("svc", &mcpsdk.ElicitRequest{
		Params: &mcpsdk.ElicitParams{
			Mode:    "url",
			Message: "Auth flow",
			URL:     "https://example.com/auth",
		},
	}, out)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if st.Mode != elicitURL {
		t.Errorf("Mode = %v, want elicitURL", st.Mode)
	}
	if st.URL != "https://example.com/auth" {
		t.Errorf("URL = %q", st.URL)
	}
	if len(st.Fields) != 0 {
		t.Errorf("URL mode should have no fields")
	}
}

func TestNewElicitState_URLModeMissingURL(t *testing.T) {
	t.Parallel()
	if _, err := newElicitState("svc", &mcpsdk.ElicitRequest{
		Params: &mcpsdk.ElicitParams{Mode: "url"},
	}, nil); err == nil {
		t.Errorf("expected error for URL mode with no URL")
	}
}

func TestNewElicitState_FormModeAllPrimitives(t *testing.T) {
	t.Parallel()
	out := make(chan *mcpsdk.ElicitResult, 1)
	st, err := newElicitState("svc", &mcpsdk.ElicitRequest{
		Params: &mcpsdk.ElicitParams{
			Message: "fill it in",
			RequestedSchema: schemaJSON(t, map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":  map[string]any{"type": "string", "description": "Your name"},
					"age":   map[string]any{"type": "integer"},
					"score": map[string]any{"type": "number"},
					"agree": map[string]any{"type": "boolean"},
					"color": map[string]any{"type": "string", "enum": []any{"red", "green", "blue"}},
				},
				"required": []any{"name"},
			}),
		},
	}, out)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got := len(st.Fields); got != 5 {
		t.Fatalf("len(Fields) = %d, want 5", got)
	}
	// Names are alpha-sorted: age, agree, color, name, score
	// ("age" before "agree" because 'e' < 'r' at index 2).
	wantOrder := []string{"age", "agree", "color", "name", "score"}
	for i, w := range wantOrder {
		if st.Fields[i].Name != w {
			t.Errorf("Fields[%d].Name = %q, want %q", i, st.Fields[i].Name, w)
		}
	}
	// First field should be focused on construction.
	if !st.Fields[0].usesInput() && len(st.Fields[0].Choices) == 0 {
		t.Errorf("first field should be a known kind")
	}
	// "name" must be marked required.
	for _, f := range st.Fields {
		if f.Name == "name" && !f.Required {
			t.Errorf("name should be required")
		}
	}
}

func TestParseSchema_RejectsNonObject(t *testing.T) {
	t.Parallel()
	_, err := parseSchema(map[string]any{"type": "array"})
	if err == nil || !strings.Contains(err.Error(), "unsupported schema type") {
		t.Errorf("expected unsupported-type error, got %v", err)
	}
}

func TestParseSchema_RejectsUnknownPropertyType(t *testing.T) {
	t.Parallel()
	_, err := parseSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"weird": map[string]any{"type": "object"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported property") {
		t.Errorf("expected unsupported-property error, got %v", err)
	}
}

func TestElicitState_NavigationWraps(t *testing.T) {
	t.Parallel()
	out := make(chan *mcpsdk.ElicitResult, 1)
	st, err := newElicitState("svc", &mcpsdk.ElicitRequest{
		Params: &mcpsdk.ElicitParams{
			RequestedSchema: schemaJSON(t, map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": "string"},
					"b": map[string]any{"type": "string"},
				},
			}),
		},
	}, out)
	if err != nil {
		t.Fatal(err)
	}
	st.nextField()
	if st.Active != 1 {
		t.Errorf("Active = %d, want 1", st.Active)
	}
	st.nextField()
	if st.Active != 0 {
		t.Errorf("Active = %d after wrap, want 0", st.Active)
	}
	st.prevField()
	if st.Active != 1 {
		t.Errorf("Active = %d after prev wrap, want 1", st.Active)
	}
}

func TestElicitField_Validate_RequiredMissing(t *testing.T) {
	t.Parallel()
	out := make(chan *mcpsdk.ElicitResult, 1)
	st, err := newElicitState("svc", &mcpsdk.ElicitRequest{
		Params: &mcpsdk.ElicitParams{
			RequestedSchema: schemaJSON(t, map[string]any{
				"type":     "object",
				"required": []any{"name"},
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
			}),
		},
	}, out)
	if err != nil {
		t.Fatal(err)
	}
	_, errMsg := st.validate()
	if errMsg == "" || !strings.Contains(errMsg, "required") {
		t.Errorf("expected required error, got %q", errMsg)
	}
}

func TestElicitField_Validate_BadInteger(t *testing.T) {
	t.Parallel()
	out := make(chan *mcpsdk.ElicitResult, 1)
	st, err := newElicitState("svc", &mcpsdk.ElicitRequest{
		Params: &mcpsdk.ElicitParams{
			RequestedSchema: schemaJSON(t, map[string]any{
				"type": "object",
				"properties": map[string]any{
					"age": map[string]any{"type": "integer"},
				},
			}),
		},
	}, out)
	if err != nil {
		t.Fatal(err)
	}
	st.Fields[0].input.SetValue("not-a-number")
	_, errMsg := st.validate()
	if errMsg == "" || !strings.Contains(errMsg, "integer") {
		t.Errorf("expected integer error, got %q", errMsg)
	}
}

func TestElicitState_AcceptCollectsValues(t *testing.T) {
	t.Parallel()
	out := make(chan *mcpsdk.ElicitResult, 1)
	st, err := newElicitState("svc", &mcpsdk.ElicitRequest{
		Params: &mcpsdk.ElicitParams{
			RequestedSchema: schemaJSON(t, map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":  map[string]any{"type": "string"},
					"agree": map[string]any{"type": "boolean"},
				},
			}),
		},
	}, out)
	if err != nil {
		t.Fatal(err)
	}
	// agree is alpha-first; set name on the second field.
	for i := range st.Fields {
		switch st.Fields[i].Name {
		case "name":
			st.Fields[i].input.SetValue("Ada")
		case "agree":
			st.Fields[i].choice = 1 // true
		}
	}
	content, errMsg := st.validate()
	if errMsg != "" {
		t.Fatalf("validate err: %s", errMsg)
	}
	if content["name"] != "Ada" {
		t.Errorf("name = %v, want Ada", content["name"])
	}
	if content["agree"] != true {
		t.Errorf("agree = %v, want true", content["agree"])
	}
}

func TestElicitState_ReplyNonBlocking(t *testing.T) {
	t.Parallel()
	out := make(chan *mcpsdk.ElicitResult, 1)
	st := &elicitState{Out: out}
	st.reply("decline", nil)
	select {
	case r := <-out:
		if r.Action != "decline" {
			t.Errorf("Action = %q, want decline", r.Action)
		}
	default:
		t.Errorf("expected a value on the reply channel")
	}
	// Second reply with a full buffer must not block (the non-blocking
	// select drops it on the floor). Run inside a goroutine and wait
	// briefly — if reply blocks, the wait times out.
	out2 := make(chan *mcpsdk.ElicitResult, 1)
	out2 <- &mcpsdk.ElicitResult{Action: "accept"}
	st2 := &elicitState{Out: out2}
	done := make(chan struct{})
	go func() {
		st2.reply("decline", nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Errorf("reply blocked when buffer was full")
	}
}
