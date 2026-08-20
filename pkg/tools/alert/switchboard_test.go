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

package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// alertToolCtx is a tool.Context that names a session, which is the
// whole point here: the switchboard template reads one. Full-interface
// satisfaction is deliberate — an ADK bump that adds a method should
// break the stub rather than silently drift (the planToolCtx pattern in
// pkg/tools).
type alertToolCtx struct {
	context.Context
	session string
}

func (c *alertToolCtx) UserContent() *genai.Content          { return nil }
func (c *alertToolCtx) InvocationID() string                 { return "test-invocation" }
func (c *alertToolCtx) AgentName() string                    { return "test-agent" }
func (c *alertToolCtx) ReadonlyState() session.ReadonlyState { return nil }
func (c *alertToolCtx) UserID() string                       { return "test-user" }
func (c *alertToolCtx) AppName() string                      { return "test-app" }
func (c *alertToolCtx) SessionID() string                    { return c.session }
func (c *alertToolCtx) Branch() string                       { return "" }
func (c *alertToolCtx) Artifacts() adkagent.Artifacts        { return nil }
func (c *alertToolCtx) State() session.State                 { return nil }
func (c *alertToolCtx) FunctionCallID() string               { return "call-1" }
func (c *alertToolCtx) Actions() *session.EventActions       { return &session.EventActions{} }
func (c *alertToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation {
	return nil
}
func (c *alertToolCtx) RequestConfirmation(string, any) error { return nil }
func (c *alertToolCtx) SearchMemory(context.Context, string) (*memory.SearchResponse, error) {
	return nil, nil
}

// inSession returns a tool.Context reporting sess.
func inSession(sess string) tool.Context {
	return &alertToolCtx{Context: context.Background(), session: sess}
}

// switchboardTarget is the shape validateAlerts accepts for the gateway:
// a conversation to post into and a bearer token to be let in with.
func switchboardTarget(name, url, conversation string) config.AlertTarget {
	return config.AlertTarget{
		Name:         name,
		URL:          url,
		Template:     config.AlertTemplateSwitchboard,
		Conversation: conversation,
		Auth:         &config.AlertAuth{BearerEnv: "SB_TOKEN"},
	}
}

func sbEnv(string) string { return "sb-secret" }

func TestRun_SwitchboardPayload(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, `{"conversation":"C0123","id":"1723742401.001900"}`, &got)
	cfg := cfgWith(switchboardTarget("chat", srv.URL, "C0123"))

	h, err := newHandler(yoloGate(t), cfg, sbEnv, nil, srv.Client())
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	res, err := h.run(inSession("s-4412"), Args{
		Target:  "chat",
		Level:   "critical",
		Summary: "checkout-svc has no healthy endpoints",
		Details: map[string]any{"incident": "INC-42", "cluster": "prod-us-east", "replicas": 0},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if got.contentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", got.contentType)
	}
	if got.authHeader != "Bearer sb-secret" {
		t.Errorf("authorization = %q, want the ingress bearer token", got.authHeader)
	}

	var payload map[string]any
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, got.body)
	}
	if payload["conversation"] != "C0123" {
		t.Errorf("conversation = %v, want the target's configured key C0123", payload["conversation"])
	}
	// Markdown, because switchboard translates it per platform. Details
	// are sorted, so the body is the same on every run.
	wantText := "**[critical]** checkout-svc has no healthy endpoints\n" +
		"- `cluster`: prod-us-east\n" +
		"- `incident`: INC-42\n" +
		"- `replicas`: 0"
	if payload["text"] != wantText {
		t.Errorf("text =\n%q\nwant\n%q", payload["text"], wantText)
	}
	// The session rides a header so the gateway can bind the thread it
	// creates back to the session that raised the alert.
	if hdr := got.header.Get(sessionHeader); hdr != "s-4412" {
		t.Errorf("%s = %q, want s-4412", sessionHeader, hdr)
	}
}

// TestRun_SwitchboardBodyIsAcceptedByAStrictDecoder is the interop test.
// switchboard's ingress decodes with DisallowUnknownFields, so any field
// this template invents — a session in the body, say — is a 400 from
// every deployed gateway rather than a field it ignores.
func TestRun_SwitchboardBodyIsAcceptedByAStrictDecoder(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "{}", &got)
	cfg := cfgWith(switchboardTarget("chat", srv.URL, "C0123"))
	h, _ := newHandler(yoloGate(t), cfg, sbEnv, nil, srv.Client())
	if _, err := h.run(inSession("s-1"), Args{
		Target: "chat", Level: "info", Summary: "hello", Details: map[string]any{"k": "v"},
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	// A copy of switchboard's messageRequest (cmd/switchboard/ingress.go).
	var req struct {
		Conversation string `json:"conversation"`
		ID           string `json:"id,omitempty"`
		Text         string `json:"text,omitempty"`
		Append       string `json:"append,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(got.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		t.Fatalf("switchboard's strict decoder would reject this body: %v (%s)", err, got.body)
	}
	if req.Conversation == "" || req.Text == "" {
		t.Errorf("decoded %+v, want conversation and text both set", req)
	}
	if req.ID != "" || req.Append != "" {
		t.Errorf("decoded %+v, want id/append left to the gateway (POST assigns the id, append is PATCH-only)", req)
	}
}

func TestRun_SwitchboardOmitsTheSessionHeaderWhenThereIsNone(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "{}", &got)
	cfg := cfgWith(switchboardTarget("chat", srv.URL, "C0123"))
	h, _ := newHandler(yoloGate(t), cfg, sbEnv, nil, srv.Client())

	// A nil tool.Context names no session; an empty header value is not
	// an id, so the header is absent rather than blank.
	if _, err := h.run(tool.Context(nil), Args{Target: "chat", Level: "info", Summary: "hi"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, has := got.header[sessionHeader]; has {
		t.Errorf("%s present with no session: %q", sessionHeader, got.header.Get(sessionHeader))
	}
}

// TestRun_GenericSendsNoSessionHeader pins the blast radius: a session id
// is internal routing detail, and a third-party webhook has no business
// learning one.
func TestRun_GenericSendsNoSessionHeader(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "ok", &got)
	cfg := cfgWith(config.AlertTarget{Name: "audit", URL: srv.URL, Template: config.AlertTemplateGeneric})
	h, _ := newHandler(yoloGate(t), cfg, nil, nil, srv.Client())
	if _, err := h.run(inSession("s-4412"), Args{Target: "audit", Level: "info", Summary: "hi"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, has := got.header[sessionHeader]; has {
		t.Errorf("generic target received %s = %q", sessionHeader, got.header.Get(sessionHeader))
	}
}

func TestSwitchboardText(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in   Args
		want string
	}{
		"no details": {
			Args{Level: "resolved", Summary: "endpoints back"},
			"**[resolved]** endpoints back",
		},
		"strings are not quoted": {
			Args{Level: "info", Summary: "s", Details: map[string]any{"ns": "kube-system"}},
			"**[info]** s\n- `ns`: kube-system",
		},
		"scalars render as JSON": {
			Args{Level: "info", Summary: "s", Details: map[string]any{"n": float64(3), "ok": false, "nil": nil}},
			"**[info]** s\n- `n`: 3\n- `nil`: null\n- `ok`: false",
		},
		"nested values render as compact JSON": {
			Args{Level: "warning", Summary: "s", Details: map[string]any{
				"pods": []any{"a", "b"},
				"meta": map[string]any{"z": 1, "a": 2},
			}},
			"**[warning]** s\n- `meta`: {\"a\":2,\"z\":1}\n- `pods`: [\"a\",\"b\"]",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := switchboardText(tc.in); got != tc.want {
				t.Errorf("switchboardText() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// TestSwitchboardText_KeyOrderIsStable guards the one thing a map can
// break silently: range order is random, so an unsorted body differs run
// to run and no receiver can diff it.
func TestSwitchboardText_KeyOrderIsStable(t *testing.T) {
	t.Parallel()
	in := Args{Level: "info", Summary: "s", Details: map[string]any{
		"delta": 4, "alpha": 1, "charlie": 3, "bravo": 2, "echo": 5, "foxtrot": 6,
	}}
	want := switchboardText(in)
	if !strings.Contains(want, "- `alpha`: 1\n- `bravo`: 2\n- `charlie`: 3") {
		t.Fatalf("details are not in sorted key order: %q", want)
	}
	for range 20 {
		if got := switchboardText(in); got != want {
			t.Fatalf("switchboardText() is not deterministic:\n%q\nvs\n%q", got, want)
		}
	}
}

// TestRenderTemplate_UnknownTemplateStillRejected keeps the
// defense-in-depth arm honest now that the switch has two live cases.
func TestRenderTemplate_UnknownTemplateStillRejected(t *testing.T) {
	t.Parallel()
	_, err := renderTemplate(config.AlertTarget{Name: "a", Template: config.AlertTemplateSlack}, Args{Level: "info", Summary: "s"}, "s-1")
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("renderTemplate(slack) = %v, want a not-implemented error", err)
	}
}
