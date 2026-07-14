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

package digest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestProcess_NilContextErrors(t *testing.T) {
	t.Parallel()
	//nolint:staticcheck // deliberately passing nil ctx to exercise the guard
	if _, err := Process(nil, []byte("x"), Options{}); err == nil {
		t.Error("expected error for nil ctx")
	}
}

func TestProcess_UnderThreshold_Passthrough(t *testing.T) {
	t.Parallel()
	res, err := Process(context.Background(), []byte(`{"k":"v"}`), Options{
		Threshold: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != MethodPassthrough {
		t.Errorf("Method = %q, want passthrough", res.Method)
	}
	if res.Digest != `{"k":"v"}` {
		t.Errorf("under-threshold payload was mangled: %q", res.Digest)
	}
	if res.RawBytes != 9 {
		t.Errorf("RawBytes = %d, want 9", res.RawBytes)
	}
}

func TestProcess_JSON_StructuralPath(t *testing.T) {
	t.Parallel()
	// A JSON payload above threshold with a long string should route
	// to the structural pruner, truncate the string, and populate
	// metadata.
	longVal := strings.Repeat("x", MaxStringChars+100)
	payload := []byte(fmt.Sprintf(`{"prose": %q}`, longVal))

	res, err := Process(context.Background(), payload, Options{
		Threshold: 100, // well below the payload size
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != MethodStructuralJSON {
		t.Errorf("Method = %q, want structural_json", res.Method)
	}
	if truncated, _ := res.Metadata["strings_truncated"].(int); truncated != 1 {
		t.Errorf("expected strings_truncated=1, got %+v", res.Metadata)
	}
	if !strings.Contains(res.Digest, "<truncated,") {
		t.Errorf("digest should carry truncation marker: %s", res.Digest)
	}
	// RawBytes reflects the original, not the digest — telemetry
	// consumers want to see the pre-prune size to compute savings.
	if res.RawBytes != len(payload) {
		t.Errorf("RawBytes = %d, want %d", res.RawBytes, len(payload))
	}
}

func TestProcess_Prose_LLMFallbackCalled(t *testing.T) {
	t.Parallel()
	// Prose payload above threshold + LLMFallback wired → the
	// fallback runs and its output becomes Digest.
	prose := strings.Repeat("word ", 500)
	var fallbackCalled bool
	fallback := func(_ context.Context, raw []byte) (string, error) {
		fallbackCalled = true
		return fmt.Sprintf("SUMMARY of %d bytes", len(raw)), nil
	}
	res, err := Process(context.Background(), []byte(prose), Options{
		Threshold:   100,
		LLMFallback: fallback,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fallbackCalled {
		t.Error("LLMFallback was never called")
	}
	if res.Method != MethodLLMFallback {
		t.Errorf("Method = %q, want llm_fallback", res.Method)
	}
	if !strings.HasPrefix(res.Digest, "SUMMARY of") {
		t.Errorf("Digest = %q, want fallback output", res.Digest)
	}
}

func TestProcess_Prose_NoLLMFallback_BoundedPassthrough(t *testing.T) {
	t.Parallel()
	// Callers who forget to wire LLMFallback still get a Result
	// they can hand to the model — bounded by MaxPassthroughBytes
	// so a megabyte of prose doesn't quietly land in the context.
	prose := strings.Repeat("a", MaxPassthroughBytes+1000)
	res, err := Process(context.Background(), []byte(prose), Options{
		Threshold: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != MethodPassthrough {
		t.Errorf("Method = %q, want passthrough (no LLMFallback)", res.Method)
	}
	if len(res.Digest) > MaxPassthroughBytes+256 { // suffix is small
		t.Errorf("passthrough digest exceeded bound: len=%d, want <= %d",
			len(res.Digest), MaxPassthroughBytes+256)
	}
	if !strings.Contains(res.Digest, "more bytes") {
		t.Errorf("bounded passthrough missing truncation marker: %q", res.Digest[len(res.Digest)-100:])
	}
}

func TestProcess_LLMFallback_ErrorDegradesToBoundedPassthrough(t *testing.T) {
	t.Parallel()
	// If the LLMFallback errors (rate limit, quota, network), the
	// caller still needs a Result — degrade to a bounded passthrough
	// and surface the error in Metadata so telemetry catches it.
	fallback := func(_ context.Context, _ []byte) (string, error) {
		return "", errors.New("rate limited")
	}
	res, err := Process(context.Background(), []byte(strings.Repeat("p", 500)), Options{
		Threshold:   100,
		LLMFallback: fallback,
	})
	if err != nil {
		t.Fatalf("Process should not surface LLMFallback error, got %v", err)
	}
	if res.Method != MethodPassthrough {
		t.Errorf("Method = %q, want passthrough (LLM errored)", res.Method)
	}
	if got, _ := res.Metadata["llm_err"].(string); got == "" {
		t.Errorf("expected llm_err in metadata: %+v", res.Metadata)
	}
}

func TestProcess_CallIDRoundTrip(t *testing.T) {
	t.Parallel()
	// Skeleton PR: no Store, but the caller's ID must round-trip so
	// the follow-up Store wiring can key on it without a signature
	// change.
	res, err := Process(context.Background(), []byte(`{"k":"v"}`), Options{
		Threshold: 0,
		CallID:    "tool-call-abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CallID != "tool-call-abc123" {
		t.Errorf("CallID = %q, want tool-call-abc123", res.CallID)
	}
}
