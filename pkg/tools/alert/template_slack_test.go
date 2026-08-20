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
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// slackBlocks renders in and decodes the result the way Slack would.
func slackBlocks(t *testing.T, in Args) map[string]any {
	t.Helper()
	out, err := renderTemplate(config.AlertTarget{Name: "sre", Template: config.AlertTemplateSlack}, in, renderEnv{})
	if err != nil {
		t.Fatalf("renderTemplate(slack): %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.body, &payload); err != nil {
		t.Fatalf("slack body is not JSON: %v (%s)", err, out.body)
	}
	return payload
}

func TestSlack_HeaderAndFields(t *testing.T) {
	t.Parallel()
	payload := slackBlocks(t, Args{
		Level:   "warning",
		Summary: "checkout-svc unresolved past budget",
		Details: map[string]any{"cluster": "prod-us-east", "attempts": 3},
	})

	// The notification fallback is not redundant with blocks: a
	// blocks-only message shows up in a phone notification as "This
	// content can't be displayed".
	if payload["text"] != "[warning] checkout-svc unresolved past budget" {
		t.Errorf("text = %v, want the level-prefixed summary", payload["text"])
	}
	blocks, _ := payload["blocks"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want a header and one section: %v", len(blocks), blocks)
	}
	header, _ := blocks[0].(map[string]any)
	if header["type"] != "header" {
		t.Errorf("first block = %v, want a header", header)
	}
	htext, _ := header["text"].(map[string]any)
	if htext["type"] != "plain_text" || htext["text"] != "[warning] checkout-svc unresolved past budget" {
		t.Errorf("header text = %v, want plain_text carrying the level + summary", htext)
	}
	section, _ := blocks[1].(map[string]any)
	fields, _ := section["fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("fields = %v, want one per detail", fields)
	}
	// Sorted: "attempts" before "cluster".
	first, _ := fields[0].(map[string]any)
	second, _ := fields[1].(map[string]any)
	if first["type"] != "mrkdwn" || first["text"] != "*attempts:*\n3" {
		t.Errorf("fields[0] = %v, want the sorted-first detail as mrkdwn", first)
	}
	if second["text"] != "*cluster:*\nprod-us-east" {
		t.Errorf("fields[1] = %v, want cluster second", second)
	}
}

func TestSlack_NoDetailsIsJustAHeader(t *testing.T) {
	t.Parallel()
	payload := slackBlocks(t, Args{Level: "info", Summary: "all clear"})
	blocks, _ := payload["blocks"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %v, want only the header", blocks)
	}
}

// TestSlack_EscapesMarkupInDetails pins the one escape that is a
// correctness issue rather than a cosmetic one: an unescaped "<" opens a
// Slack link and swallows the rest of the value, so "<none>" — what
// kubectl prints for an empty field — would render as nothing at all.
func TestSlack_EscapesMarkupInDetails(t *testing.T) {
	t.Parallel()
	payload := slackBlocks(t, Args{
		Level:   "critical",
		Summary: "endpoints missing",
		Details: map[string]any{"endpoints": "<none>", "selector": "a=b & c>d"},
	})
	blocks, _ := payload["blocks"].([]any)
	section, _ := blocks[1].(map[string]any)
	fields, _ := section["fields"].([]any)
	first, _ := fields[0].(map[string]any)
	if first["text"] != "*endpoints:*\n&lt;none&gt;" {
		t.Errorf("fields[0] = %v, want < and > entity-escaped", first)
	}
	second, _ := fields[1].(map[string]any)
	if second["text"] != "*selector:*\na=b &amp; c&gt;d" {
		t.Errorf("fields[1] = %v, want & and > entity-escaped", second)
	}
	// The header is plain_text, which Slack renders literally — escaping
	// it would show the entities to the reader.
	header, _ := blocks[0].(map[string]any)
	htext, _ := header["text"].(map[string]any)
	if strings.Contains(htext["text"].(string), "&") {
		t.Errorf("header = %v, want plain_text left unescaped", htext)
	}
}

// TestSlack_HeaderIsTruncatedToSlacksLimit is the bug this template
// would otherwise ship with: Slack rejects the WHOLE message with
// invalid_blocks when a header exceeds 150 characters, and a model
// writing a long one-line summary is not an edge case.
func TestSlack_HeaderIsTruncatedToSlacksLimit(t *testing.T) {
	t.Parallel()
	payload := slackBlocks(t, Args{Level: "critical", Summary: strings.Repeat("x", 400)})
	blocks, _ := payload["blocks"].([]any)
	header, _ := blocks[0].(map[string]any)
	htext, _ := header["text"].(map[string]any)
	got := htext["text"].(string)
	if n := len([]rune(got)); n != slackHeaderMax {
		t.Errorf("header length = %d, want %d", n, slackHeaderMax)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("header = %q, want the cut marked with an ellipsis", got)
	}
	// The fallback text has a far bigger budget, so it keeps more.
	if n := len([]rune(payload["text"].(string))); n <= slackHeaderMax {
		t.Errorf("fallback text length = %d, want the longer fallback budget used", n)
	}
}

func TestSlack_LongFieldIsTruncated(t *testing.T) {
	t.Parallel()
	payload := slackBlocks(t, Args{
		Level:   "info",
		Summary: "s",
		Details: map[string]any{"log": strings.Repeat("y", 5000)},
	})
	blocks, _ := payload["blocks"].([]any)
	section, _ := blocks[1].(map[string]any)
	fields, _ := section["fields"].([]any)
	first, _ := fields[0].(map[string]any)
	if n := len([]rune(first["text"].(string))); n != slackTextMax {
		t.Errorf("field length = %d, want %d", n, slackTextMax)
	}
}

// TestSlack_ChunksFieldsIntoSections covers the second hard limit:
// a section holds at most 10 fields.
func TestSlack_ChunksFieldsIntoSections(t *testing.T) {
	t.Parallel()
	details := map[string]any{}
	for i := range 23 {
		details[fmt.Sprintf("k%02d", i)] = i
	}
	payload := slackBlocks(t, Args{Level: "info", Summary: "s", Details: details})
	blocks, _ := payload["blocks"].([]any)
	if len(blocks) != 4 { // header + 10 + 10 + 3
		t.Fatalf("blocks = %d, want header plus three sections", len(blocks))
	}
	for i, want := range []int{10, 10, 3} {
		section, _ := blocks[i+1].(map[string]any)
		fields, _ := section["fields"].([]any)
		if len(fields) != want {
			t.Errorf("section %d has %d fields, want %d", i, len(fields), want)
		}
	}
}

// TestSlack_OverflowIsNamedNotSilent — a truncated alert that does not
// say it was truncated is worse than one that does.
func TestSlack_OverflowIsNamedNotSilent(t *testing.T) {
	t.Parallel()
	details := map[string]any{}
	for i := range slackMaxFields + 7 {
		details[fmt.Sprintf("k%04d", i)] = i
	}
	payload := slackBlocks(t, Args{Level: "info", Summary: "s", Details: details})
	blocks, _ := payload["blocks"].([]any)
	if len(blocks) > slackMaxBlocks {
		t.Fatalf("blocks = %d, want at most Slack's %d", len(blocks), slackMaxBlocks)
	}
	last, _ := blocks[len(blocks)-1].(map[string]any)
	if last["type"] != "context" {
		t.Fatalf("last block = %v, want a context block naming the omission", last)
	}
	elems, _ := last["elements"].([]any)
	elem, _ := elems[0].(map[string]any)
	if !strings.Contains(elem["text"].(string), "7 more detail(s) omitted") {
		t.Errorf("overflow note = %v, want the omitted count", elem)
	}
}

// TestRun_SlackSendsNoSessionHeader keeps #798's blast radius pinned as
// templates are added: a session id is internal routing detail and a
// third-party webhook has no reason to learn one.
func TestRun_SlackSendsNoSessionHeader(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "ok", &got)
	cfg := cfgWith(config.AlertTarget{Name: "sre", URL: srv.URL, Template: config.AlertTemplateSlack})
	h, err := newHandler(yoloGate(t), cfg, func(string) string { return "" }, nil, srv.Client())
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	if _, err := h.run(inSession("s-4412"), Args{Target: "sre", Level: "info", Summary: "hi"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if v := got.header.Get(sessionHeader); v != "" {
		t.Errorf("%s = %q on a slack target, want it absent", sessionHeader, v)
	}
	if got.authHeader != "" {
		t.Errorf("authorization = %q, want none — the webhook URL is the credential", got.authHeader)
	}
}
