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

package anthropic

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
)

// longHistory builds n alternating user/model turns, one text part each,
// so every content block is markable and the stride math is readable.
func longHistory(n int) []*genai.Content {
	out := make([]*genai.Content, 0, n)
	for i := range n {
		role := genai.RoleUser
		if i%2 == 1 {
			role = genai.RoleModel
		}
		out = append(out, &genai.Content{
			Role:  role,
			Parts: []*genai.Part{{Text: "turn"}},
		})
	}
	return out
}

// markedBlocks reports, for each message, which block indices carry a
// cache_control marker.
func markedBlocks(msgs []anthropic.MessageParam) [][]int {
	out := make([][]int, len(msgs))
	for i, m := range msgs {
		for j, b := range m.Content {
			if cc := b.GetCacheControl(); cc != nil && cc.Type != "" {
				out[i] = append(out[i], j)
			}
		}
	}
	return out
}

func countMarked(msgs []anthropic.MessageParam) int {
	n := 0
	for _, idx := range markedBlocks(msgs) {
		n += len(idx)
	}
	return n
}

func systemMarked(sys []anthropic.TextBlockParam) int {
	n := 0
	for _, b := range sys {
		if b.CacheControl.Type != "" {
			n++
		}
	}
	return n
}

func TestApplyCacheBreakpoints_Disabled(t *testing.T) {
	t.Parallel()
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("be helpful", genai.RoleUser),
	}
	p, err := buildParams("claude-opus-4-7", longHistory(40), cfg, CacheOptions{}, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if got := systemMarked(p.System); got != 0 {
		t.Errorf("system markers = %d, want 0 with caching off", got)
	}
	if got := countMarked(p.Messages); got != 0 {
		t.Errorf("history markers = %d, want 0 with caching off", got)
	}
}

func TestApplyCacheBreakpoints_SystemMarksLastBlockOnly(t *testing.T) {
	t.Parallel()
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{{Text: "one"}, {Text: "two"}, {Text: "three"}},
		},
	}
	p, err := buildParams("claude-opus-4-7", nil, cfg, CacheOptions{System: true}, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(p.System) != 3 {
		t.Fatalf("system blocks = %d, want 3", len(p.System))
	}
	// The marker declares "cache the prefix ending HERE". Anything but
	// the last block would leave the tail of the system prompt — and the
	// whole conversation — outside the entry.
	for i, b := range p.System[:2] {
		if b.CacheControl.Type != "" {
			t.Errorf("system[%d] carries a marker; only the last block should", i)
		}
	}
	if p.System[2].CacheControl.Type == "" {
		t.Error("last system block carries no marker")
	}
}

func TestApplyCacheBreakpoints_SystemOnlyLeavesHistoryClean(t *testing.T) {
	t.Parallel()
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("be helpful", genai.RoleUser),
	}
	p, err := buildParams("claude-opus-4-7", longHistory(40), cfg, CacheOptions{System: true}, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if got := countMarked(p.Messages); got != 0 {
		t.Errorf("history markers = %d, want 0 when only System is set", got)
	}
}

func TestMarkHistoryBreakpoints_RollsWithTheTail(t *testing.T) {
	t.Parallel()
	// 50 blocks, no system marker: budget is the full 4.
	p, err := buildParams("claude-opus-4-7", longHistory(50), nil, CacheOptions{History: true}, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	marks := markedBlocks(p.Messages)
	var at []int
	for i, idx := range marks {
		if len(idx) > 0 {
			at = append(at, i)
		}
	}
	// Backward walk from the last block: mark 49, skip 15, mark 33, and
	// so on — markers 16 apart, comfortably inside the API's 20-block
	// lookback, which is what keeps the chain to the previous turn's
	// entry alive.
	want := []int{1, 17, 33, 49}
	if len(at) != len(want) {
		t.Fatalf("marked messages = %v, want %v", at, want)
	}
	for i := range want {
		if at[i] != want[i] {
			t.Fatalf("marked messages = %v, want %v", at, want)
		}
	}
	// The LAST block must always carry one: that is the marker the next
	// request reads back. Marking only earlier positions would pin the
	// cache and re-bill the growing tail forever.
	if len(marks[len(marks)-1]) == 0 {
		t.Error("last history block carries no marker")
	}
}

func TestMarkHistoryBreakpoints_NeverExceedsTheAPILimit(t *testing.T) {
	t.Parallel()
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("be helpful", genai.RoleUser),
	}
	for _, turns := range []int{1, 2, 16, 60, 300, 1000} {
		p, err := buildParams("claude-opus-4-7", longHistory(turns), cfg, DefaultCacheOptions(), BuiltinTools{})
		if err != nil {
			t.Fatalf("buildParams(%d turns): %v", turns, err)
		}
		// A fifth cache_control is a 400 from the API, not a silent
		// drop, so this bound is a hard correctness property.
		// Against the literal 4, not against maxCacheBreakpoints: the
		// constant is exactly the thing that would be wrong, and an
		// assertion phrased in terms of it can't catch that.
		total := systemMarked(p.System) + countMarked(p.Messages)
		if total > 4 {
			t.Errorf("%d turns: %d breakpoints, API allows at most 4", turns, total)
		}
		if systemMarked(p.System) != 1 {
			t.Errorf("%d turns: system marker missing — history must not starve the prefix marker", turns)
		}
	}
}

// TestMarkHistoryBreakpoints_ToleratesA52BlockTurn pins how much one
// agentic step may append before the chain to the previous request's
// entry breaks. With the system marker taking one of the four slots,
// history markers land at end-offsets 0, 16 and 32; the deepest
// lookback window therefore reaches 32+20 = 52 blocks back. A turn that
// appends 53 blocks (a ~26-wide parallel tool fan-out) leaves every old
// entry out of reach and re-writes the whole message history at 1.25x —
// documented in docs/anthropic-prompt-caching-design.md, and this test
// is what would notice if a stride change moved the boundary.
func TestMarkHistoryBreakpoints_ToleratesA52BlockTurn(t *testing.T) {
	t.Parallel()
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("be helpful", genai.RoleUser),
	}
	p, err := buildParams("claude-opus-4-7", longHistory(80), cfg, DefaultCacheOptions(), BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	marks := markedBlocks(p.Messages)
	last := len(marks) - 1
	var offsets []int
	for i := last; i >= 0; i-- {
		if len(marks[i]) > 0 {
			offsets = append(offsets, last-i)
		}
	}
	want := []int{0, 16, 32}
	if len(offsets) != len(want) {
		t.Fatalf("marker end-offsets = %v, want %v", offsets, want)
	}
	for i := range want {
		if offsets[i] != want[i] {
			t.Fatalf("marker end-offsets = %v, want %v", offsets, want)
		}
	}
	if tolerance := offsets[len(offsets)-1] + 20; tolerance != 52 {
		t.Errorf("survivable turn size = %d blocks, want 52", tolerance)
	}
}

func TestMarkHistoryBreakpoints_ShortHistoryMarksOnce(t *testing.T) {
	t.Parallel()
	p, err := buildParams("claude-opus-4-7", longHistory(3), nil, CacheOptions{History: true}, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if got := countMarked(p.Messages); got != 1 {
		t.Errorf("markers = %d, want 1 (a 3-block history needs one, at the end)", got)
	}
}

func TestMarkHistoryBreakpoints_EmptyHistory(t *testing.T) {
	t.Parallel()
	p, err := buildParams("claude-opus-4-7", nil, nil, DefaultCacheOptions(), BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if got := countMarked(p.Messages); got != 0 {
		t.Errorf("markers = %d on an empty history, want 0", got)
	}
}

func TestMarkHistoryBreakpoints_SkipsThinkingBlocks(t *testing.T) {
	t.Parallel()
	// A thinking block has no cache_control field in the API. Marking it
	// would be dropped on the floor at serialization, silently costing
	// the request a breakpoint.
	msgs := []anthropic.MessageParam{{
		Role: anthropic.MessageParamRoleAssistant,
		Content: []anthropic.ContentBlockParamUnion{
			anthropic.NewTextBlock("before"),
			anthropic.NewThinkingBlock("sig", "pondering"),
		},
	}}
	if got := markHistoryBreakpoints(msgs, 4); got != 1 {
		t.Fatalf("placed = %d, want 1", got)
	}
	if msgs[0].Content[1].GetCacheControl() != nil {
		t.Error("thinking block is markable; the SDK contract this skip relies on has changed")
	}
	if cc := msgs[0].Content[0].GetCacheControl(); cc == nil || cc.Type == "" {
		t.Error("marker did not fall back to the preceding markable block")
	}
}

func TestMarkHistoryBreakpoints_ZeroBudget(t *testing.T) {
	t.Parallel()
	msgs := []anthropic.MessageParam{{
		Role:    anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock("hi")},
	}}
	if got := markHistoryBreakpoints(msgs, 0); got != 0 {
		t.Errorf("placed = %d with a zero budget, want 0", got)
	}
	if cc := msgs[0].Content[0].GetCacheControl(); cc != nil && cc.Type != "" {
		t.Error("a zero budget still marked a block")
	}
}

// TestSetCacheControl_MutatesThroughAUnionCopy pins the SDK behaviour
// the whole marker scheme rests on: ContentBlockParamUnion is passed by
// value, so if GetCacheControl returned a pointer into the copy, every
// marker would be written to a temporary and the request would ship
// with none.
func TestSetCacheControl_MutatesThroughAUnionCopy(t *testing.T) {
	t.Parallel()
	blocks := []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock("hi")}
	if !setCacheControl(blocks[0]) {
		t.Fatal("setCacheControl reported a text block as unmarkable")
	}
	if cc := blocks[0].GetCacheControl(); cc == nil || cc.Type == "" {
		t.Fatal("marker did not survive; GetCacheControl no longer aliases the caller's block")
	}
}

func TestBuildParams_MarkersUseTheFiveMinuteTTL(t *testing.T) {
	t.Parallel()
	// The cost meter has exactly one write rate (the 5-minute 1.25x
	// one). A "1h" TTL here bills 2x and would understate every cached
	// turn by 37.5% — see #770.
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("be helpful", genai.RoleUser),
	}
	p, err := buildParams("claude-opus-4-7", longHistory(4), cfg, DefaultCacheOptions(), BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if ttl := p.System[0].CacheControl.TTL; ttl != "" {
		t.Errorf("system TTL = %q, want unset (the API default is 5m)", ttl)
	}
	for i, m := range p.Messages {
		for j, b := range m.Content {
			cc := b.GetCacheControl()
			if cc == nil || cc.Type == "" {
				continue
			}
			if cc.TTL != "" {
				t.Errorf("messages[%d].content[%d] TTL = %q, want unset until the catalog carries a 1h rate (#770)", i, j, cc.TTL)
			}
		}
	}
}

func TestCacheOptionsFromConfig(t *testing.T) {
	t.Parallel()
	off, on := false, true
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want CacheOptions
	}{
		{"nil config", nil, DefaultCacheOptions()},
		{"no anthropic block", &config.Config{}, DefaultCacheOptions()},
		{"no prompt_cache block", withAnthropic(&config.AnthropicConfig{}), DefaultCacheOptions()},
		{"enabled unset", withAnthropic(&config.AnthropicConfig{PromptCache: &config.PromptCacheConfig{}}), DefaultCacheOptions()},
		{"enabled true", withAnthropic(&config.AnthropicConfig{PromptCache: &config.PromptCacheConfig{Enabled: &on}}), DefaultCacheOptions()},
		{"enabled false", withAnthropic(&config.AnthropicConfig{PromptCache: &config.PromptCacheConfig{Enabled: &off}}), CacheOptions{}},
	} {
		if got := cacheOptionsFromConfig(tc.cfg); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func withAnthropic(a *config.AnthropicConfig) *config.Config {
	c := &config.Config{}
	c.Model.Anthropic = a
	return c
}

// TestGenerateContent_PauseTurnRemarksTheGrownRequest covers the one
// place a request grows after buildParams has already marked it: the
// pause_turn continuation appends the paused assistant turn — server
// tool blocks and all — and re-issues.
//
// Two things must hold on request #2. The marker count must still be
// within the API's limit of 4 (a naive second pass would stack four
// more onto the existing ones and earn a 400), and the tail marker must
// have MOVED onto the appended turn — otherwise the replayed
// server-tool payload is re-sent at full rate every continuation and
// the chain drifts out of the next request's lookback window.
func TestGenerateContent_PauseTurnRemarksTheGrownRequest(t *testing.T) {
	t.Parallel()
	l, captured := newOfflineLLMSeq(t, "claude-test",
		[]string{pauseTurnSSEFixture, webSearchDoneSSEFixture})
	l.cache = DefaultCacheOptions()

	drain(t, l, context.Background(), &adkmodel.LLMRequest{
		Contents: longHistory(6),
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("be helpful", genai.RoleUser),
		},
	})

	reqs := *captured
	if len(reqs) != 2 {
		t.Fatalf("adapter issued %d requests, want 2 (initial + pause_turn continuation)", len(reqs))
	}
	for i, r := range reqs {
		if n := countCacheControl(r.body); n == 0 || n > 4 {
			t.Errorf("request %d carried %d cache_control markers, want 1..4", i, n)
		}
	}
	if got, want := markedMessages(t, reqs[1].body), len(messages(t, reqs[1].body))-1; !contains(got, want) {
		t.Errorf("continuation marked messages %v; the replayed assistant turn (index %d) carries no marker, so its server-tool blocks are re-sent at full rate", got, want)
	}
}

// messages pulls the decoded "messages" array out of a captured body.
func messages(t *testing.T, body map[string]any) []any {
	t.Helper()
	msgs, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("request body has no messages array: %v", body)
	}
	return msgs
}

// markedMessages reports which message indices carry at least one
// cache_control marker, read off the wire rather than the param structs.
func markedMessages(t *testing.T, body map[string]any) []int {
	t.Helper()
	var out []int
	for i, m := range messages(t, body) {
		if countCacheControl(map[string]any{"m": m}) > 0 {
			out = append(out, i)
		}
	}
	return out
}

func contains(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestWithCacheSystem_StillMeansNoCachingWhenFalse pins the compat
// contract for the pre-#714 option. It predates the history
// breakpoints, so a library consumer who wrote WithCacheSystem(false)
// to avoid the write premium must keep that outcome — not silently
// acquire rolling history markers from the new defaults.
func TestWithCacheSystem_StillMeansNoCachingWhenFalse(t *testing.T) {
	t.Parallel()
	off, err := New("test-key-not-real", WithCacheSystem(false))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := off.PromptCache(); got.Enabled() {
		t.Errorf("WithCacheSystem(false) left %+v, want caching fully off", got)
	}
	on, err := New("test-key-not-real", WithCacheSystem(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := on.PromptCache(); !got.System || got.History {
		t.Errorf("WithCacheSystem(true) left %+v, want the system marker only", got)
	}
}

// TestGenerateContent_ContextOptOut is the end-to-end check that a
// one-shot caller (summarizer / checkpointer / /btw) pays no write
// premium: same llm, same request, only the context differs.
func TestGenerateContent_ContextOptOut(t *testing.T) {
	t.Parallel()
	req := &adkmodel.LLMRequest{
		Contents: longHistory(6),
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("summarize", genai.RoleUser),
		},
	}

	l, captured := newOfflineLLM(t, "claude-test", cacheWarmingSSEFixture)
	l.cache = DefaultCacheOptions()
	drain(t, l, context.Background(), req)
	if n := countCacheControl(captured.body); n == 0 {
		t.Fatal("baseline request carried no cache_control; the opt-out test proves nothing")
	}

	l2, captured2 := newOfflineLLM(t, "claude-test", cacheWarmingSSEFixture)
	l2.cache = DefaultCacheOptions()
	drain(t, l2, models.WithoutPromptCache(context.Background()), req)
	if n := countCacheControl(captured2.body); n != 0 {
		t.Errorf("suppressed request carried %d cache_control markers, want 0", n)
	}
}

func drain(t *testing.T, l *llm, ctx context.Context, req *adkmodel.LLMRequest) {
	t.Helper()
	for _, err := range l.GenerateContent(ctx, req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
	}
}

// countCacheControl walks the decoded request body counting
// "cache_control" keys — the wire-level truth, independent of which SDK
// struct carried them.
func countCacheControl(body map[string]any) int {
	var walk func(any) int
	walk = func(v any) int {
		switch t := v.(type) {
		case map[string]any:
			n := 0
			for k, sub := range t {
				if k == "cache_control" {
					n++
					continue
				}
				n += walk(sub)
			}
			return n
		case []any:
			n := 0
			for _, sub := range t {
				n += walk(sub)
			}
			return n
		}
		return 0
	}
	return walk(body)
}
