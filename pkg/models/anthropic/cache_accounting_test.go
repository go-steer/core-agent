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

// Cache-write reporting (#263). genai's UsageMetadata has two input
// buckets and Anthropic reports three, so cache_creation_input_tokens
// rides out-of-band on CustomMetadata. Without it the tracker folds
// written tokens into the uncached remainder and bills them at 1x
// instead of the 1.25x Anthropic charges.

package anthropic

import (
	"context"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// cacheWarmingSSEFixture is a turn that both READ from and WROTE to the
// prompt cache — the shape every first request after a prefix change
// takes. Anthropic reports the three input buckets as disjoint:
// 1,000 fresh + 20,000 read + 4,000 written.
const cacheWarmingSSEFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_cache_01","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1000,"cache_creation_input_tokens":4000,"cache_read_input_tokens":20000,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"warm"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":500}}

event: message_stop
data: {"type":"message_stop"}

`

// cacheWritingPauseTurnSSEFixture is request #1 of a paused turn that
// wrote 4,000 tokens of cache; the continuation below writes 1,500
// more. addUsage must sum both into the one terminal response.
const cacheWritingPauseTurnSSEFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_cache_pause_01","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1000,"cache_creation_input_tokens":4000,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_c1","name":"web_search","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\": \"go\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"pause_turn","stop_sequence":null},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

// cacheWritingResumeSSEFixture is request #2 of the paused turn.
const cacheWritingResumeSSEFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_cache_pause_02","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":200,"cache_creation_input_tokens":1500,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":20}}

event: message_stop
data: {"type":"message_stop"}

`

// terminalOf drains GenerateContent and returns the single
// TurnComplete response, failing the test on any error.
func terminalOf(t *testing.T, l *llm) *adkmodel.LLMResponse {
	t.Helper()
	var terminal *adkmodel.LLMResponse
	for resp, err := range l.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Contents: userText("hi"),
	}, true) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		if resp.TurnComplete {
			terminal = resp
		}
	}
	if terminal == nil {
		t.Fatal("no TurnComplete response")
	}
	return terminal
}

// TestGenerateContent_StampsCacheWriteSidecar pins that a turn which
// wrote cache entries reports the write count on CustomMetadata, and
// that the count is the one pkg/usage reads back.
func TestGenerateContent_StampsCacheWriteSidecar(t *testing.T) {
	t.Parallel()
	l, _ := newOfflineLLM(t, "claude-test", cacheWarmingSSEFixture)
	terminal := terminalOf(t, l)

	// The genai-shaped buckets are unchanged: total prompt is the sum
	// of all three, CachedContentTokenCount is the read subset only.
	u := terminal.UsageMetadata
	if u == nil {
		t.Fatal("terminal response carries no UsageMetadata")
	}
	if u.PromptTokenCount != 25_000 {
		t.Errorf("PromptTokenCount = %d, want 25000 (1000 fresh + 20000 read + 4000 written)", u.PromptTokenCount)
	}
	if u.CachedContentTokenCount != 20_000 {
		t.Errorf("CachedContentTokenCount = %d, want 20000 (cache reads only)", u.CachedContentTokenCount)
	}

	// The write bucket, which UsageMetadata has no field for.
	got := usage.TurnUsageFromMetadata(u, terminal.CustomMetadata)
	if got.CacheCreationInputTokens != 4_000 {
		t.Errorf("CacheCreationInputTokens = %d, want 4000; CustomMetadata = %#v",
			got.CacheCreationInputTokens, terminal.CustomMetadata)
	}
	if got.UncachedInputTokens() != 1_000 {
		t.Errorf("UncachedInputTokens() = %d, want 1000 — written tokens must not land in the uncached bucket",
			got.UncachedInputTokens())
	}
}

// TestGenerateContent_NoCacheWritesLeavesSidecarNil pins that ordinary
// turns don't get an all-zero map stamped onto every event. A nil
// sidecar reads back as zero writes, which is the correct answer for
// every provider that doesn't bill writes per token.
func TestGenerateContent_NoCacheWritesLeavesSidecarNil(t *testing.T) {
	t.Parallel()
	l, _ := newOfflineLLM(t, "claude-test", messagesSSEFixture)
	terminal := terminalOf(t, l)

	if terminal.CustomMetadata != nil {
		t.Errorf("CustomMetadata = %#v, want nil on a turn that wrote no cache", terminal.CustomMetadata)
	}
	if got := usage.TurnUsageFromMetadata(terminal.UsageMetadata, terminal.CustomMetadata); got.CacheCreationInputTokens != 0 {
		t.Errorf("CacheCreationInputTokens = %d, want 0", got.CacheCreationInputTokens)
	}
}

// TestGenerateContent_CacheWritesSumAcrossPauseTurn pins that a turn
// spanning several requests reports the TOTAL written tokens. The turn
// yields exactly one terminal response, so a sidecar carrying only the
// last request's count would under-bill every paused turn.
func TestGenerateContent_CacheWritesSumAcrossPauseTurn(t *testing.T) {
	t.Parallel()
	l, captured := newOfflineLLMSeq(t, "claude-test",
		[]string{cacheWritingPauseTurnSSEFixture, cacheWritingResumeSSEFixture})
	terminal := terminalOf(t, l)

	if n := len(*captured); n != 2 {
		t.Fatalf("issued %d requests, want 2 (pause_turn continuation)", n)
	}
	got := usage.TurnUsageFromMetadata(terminal.UsageMetadata, terminal.CustomMetadata)
	if got.CacheCreationInputTokens != 5_500 {
		t.Errorf("CacheCreationInputTokens = %d, want 5500 (4000 + 1500 across both requests)",
			got.CacheCreationInputTokens)
	}
	if got.InputTokens != 1_000+4_000+200+1_500 {
		t.Errorf("InputTokens = %d, want 6700 (all buckets, both requests)", got.InputTokens)
	}
}

// TestCacheWriteSidecar_SurvivesTheEventSeam walks the sidecar the rest
// of the way to the meter: the runner copies the whole LLMResponse into
// the session.Event it emits (adk internal/llminternal/base_flow.go),
// and every cost path reads the event, not the response. The embed is
// the one hop this package can't see, so pin that a byte-for-byte copy
// still prices the write bucket — a future struct-field copy that
// enumerated fields and forgot CustomMetadata would restore the
// undercount with nothing else failing.
func TestCacheWriteSidecar_SurvivesTheEventSeam(t *testing.T) {
	t.Parallel()
	l, _ := newOfflineLLM(t, "claude-test", cacheWarmingSSEFixture)
	terminal := terminalOf(t, l)

	var tap usage.TurnTap
	ev := &session.Event{LLMResponse: *terminal}
	tap.Observe(ev)
	got, ok := tap.Commit(ev)
	if !ok {
		t.Fatal("TurnTap did not commit the terminal event")
	}
	if got.CacheCreationInputTokens != 4_000 {
		t.Errorf("CacheCreationInputTokens = %d, want 4000; event CustomMetadata = %#v",
			got.CacheCreationInputTokens, ev.CustomMetadata)
	}
	if got.UncachedInputTokens() != 1_000 {
		t.Errorf("UncachedInputTokens() = %d, want 1000", got.UncachedInputTokens())
	}
}

// TestCacheCreationMetadata_Shape pins the sidecar's key and value type
// directly, so a rename on either side of the pkg/usage boundary breaks
// here rather than silently reverting cost to the undercount.
func TestCacheCreationMetadata_Shape(t *testing.T) {
	t.Parallel()
	// The provider spells the key literally to stay off the accounting
	// layer's dependency graph; this is the check that keeps the two
	// spellings identical.
	if cacheCreationTokensMetadataKey != usage.CacheCreationTokensMetadataKey {
		t.Fatalf("provider key %q != usage.CacheCreationTokensMetadataKey %q",
			cacheCreationTokensMetadataKey, usage.CacheCreationTokensMetadataKey)
	}
	if got := cacheCreationMetadata(sdk.Usage{CacheCreationInputTokens: 0}); got != nil {
		t.Errorf("cacheCreationMetadata(0) = %#v, want nil", got)
	}
	m := cacheCreationMetadata(sdk.Usage{CacheCreationInputTokens: 4_000})
	if len(m) != 1 {
		t.Fatalf("sidecar = %#v, want exactly one key", m)
	}
	v, ok := m[usage.CacheCreationTokensMetadataKey].(int64)
	if !ok || v != 4_000 {
		t.Errorf("sidecar[%q] = %#v, want int64(4000)", usage.CacheCreationTokensMetadataKey,
			m[usage.CacheCreationTokensMetadataKey])
	}
}
