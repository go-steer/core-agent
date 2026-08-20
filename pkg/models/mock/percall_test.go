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

package mock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// twoTurnScript is the fan-out fixture: the same two-turn transcript
// every caller is expected to replay from the top.
const twoTurnScript = `{"request":{"model":"m"},"responses":[{"content":{"role":"model","parts":[{"text":"first"}]},"turnComplete":true}]}
{"request":{"model":"m"},"responses":[{"content":{"role":"model","parts":[{"text":"second"}]},"turnComplete":true}]}
`

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// firstText drains one GenerateContent call and returns the text of
// the first response. Unlike drain it reports errors rather than
// calling t.Fatalf, so it is safe to call from a goroutine.
func firstText(llm adkmodel.LLM) (string, error) {
	for resp, err := range llm.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		if err != nil {
			return "", err
		}
		return resp.Content.Parts[0].Text, nil
	}
	return "", nil
}

func TestNewScriptedPerCall_EveryModelCallStartsAtTurnZero(t *testing.T) {
	t.Parallel()
	p, err := NewScriptedPerCall(writeScript(t, twoTurnScript), false)
	if err != nil {
		t.Fatalf("NewScriptedPerCall: %v", err)
	}
	for i := range 3 {
		llm, err := p.Model(context.Background(), "")
		if err != nil {
			t.Fatalf("Model %d: %v", i, err)
		}
		got := drain(t, llm, &adkmodel.LLMRequest{})
		if got[0].Content.Parts[0].Text != "first" {
			t.Errorf("Model call %d replayed %q, want the script from the top (%q)",
				i, got[0].Content.Parts[0].Text, "first")
		}
	}
}

// TestNewScripted_SharesOneCursor pins the behaviour NewScriptedPerCall
// exists to avoid, so the contrast the docs draw stays true. Two Model
// calls on a NewScripted provider walk ONE cursor: the second caller
// picks up where the first left off.
func TestNewScripted_SharesOneCursor(t *testing.T) {
	t.Parallel()
	p, err := NewScripted(writeScript(t, twoTurnScript), false)
	if err != nil {
		t.Fatalf("NewScripted: %v", err)
	}
	a, err := p.Model(context.Background(), "")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	b, err := p.Model(context.Background(), "")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	if drain(t, a, &adkmodel.LLMRequest{})[0].Content.Parts[0].Text != "first" {
		t.Fatal("first caller should get turn 0")
	}
	if got := drain(t, b, &adkmodel.LLMRequest{})[0].Content.Parts[0].Text; got != "second" {
		t.Errorf("NewScripted's second Model call replayed %q, want %q — the shared cursor is the documented behaviour", got, "second")
	}
}

// TestNewScriptedPerCall_ConcurrentCallersEachReplayTheWholeScript is
// the fan-out case: N children ask for a model at once and each must
// see the transcript in full, in order. With a shared cursor they
// would divide the two turns between them and the rest would hit
// "script exhausted".
func TestNewScriptedPerCall_ConcurrentCallersEachReplayTheWholeScript(t *testing.T) {
	t.Parallel()
	p, err := NewScriptedPerCall(writeScript(t, twoTurnScript), false)
	if err != nil {
		t.Fatalf("NewScriptedPerCall: %v", err)
	}

	const children = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []string
	)
	seen := make([][]string, children)
	for i := range children {
		wg.Add(1)
		go func() {
			defer wg.Done()
			llm, err := p.Model(context.Background(), "")
			if err != nil {
				mu.Lock()
				errs = append(errs, "Model: "+err.Error())
				mu.Unlock()
				return
			}
			var texts []string
			for range 2 {
				text, err := firstText(llm)
				if err != nil {
					mu.Lock()
					errs = append(errs, "child replay: "+err.Error())
					mu.Unlock()
					return
				}
				texts = append(texts, text)
			}
			seen[i] = texts
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("concurrent replay errored: %s", strings.Join(errs, "; "))
	}
	for i, texts := range seen {
		if len(texts) != 2 || texts[0] != "first" || texts[1] != "second" {
			t.Errorf("child %d saw %v, want [first second]", i, texts)
		}
	}
}

// TestNewScriptedPerCall_CallersDoNotShareResponsePointers guards the
// re-decode. Recorded responses go straight into the agent loop, which
// is free to write to them; two children handed the same pointer would
// corrupt each other's replay in a way no cursor fix would catch.
func TestNewScriptedPerCall_CallersDoNotShareResponsePointers(t *testing.T) {
	t.Parallel()
	p, err := NewScriptedPerCall(writeScript(t, twoTurnScript), false)
	if err != nil {
		t.Fatalf("NewScriptedPerCall: %v", err)
	}
	a, err := p.Model(context.Background(), "")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	b, err := p.Model(context.Background(), "")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}

	got := drain(t, a, &adkmodel.LLMRequest{})
	got[0].Content.Parts[0].Text = "clobbered by the first caller"

	if text := drain(t, b, &adkmodel.LLMRequest{})[0].Content.Parts[0].Text; text != "first" {
		t.Errorf("second caller saw %q — the two replays are aliasing the same response", text)
	}
}

func TestNewScriptedPerCall_MalformedTranscriptFailsAtConstruction(t *testing.T) {
	t.Parallel()
	_, err := NewScriptedPerCall(writeScript(t, "not json\n"), false)
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("want a line-1 parse error from the constructor, got %v", err)
	}
}

func TestNewScriptedPerCall_MissingFileFailsAtConstruction(t *testing.T) {
	t.Parallel()
	_, err := NewScriptedPerCall(filepath.Join(t.TempDir(), "absent.jsonl"), false)
	if err == nil || !strings.Contains(err.Error(), "scripted: open") {
		t.Errorf("want an open error from the constructor, got %v", err)
	}
}

// TestNewScriptedPerCall_LaterEditsToTheFileDoNotChangeTheReplay is
// why the bytes are read once up front rather than per Model call: a
// fan-out that reopened the file would let a mid-run edit give two
// children different scripts.
func TestNewScriptedPerCall_LaterEditsToTheFileDoNotChangeTheReplay(t *testing.T) {
	t.Parallel()
	path := writeScript(t, twoTurnScript)
	p, err := NewScriptedPerCall(path, false)
	if err != nil {
		t.Fatalf("NewScriptedPerCall: %v", err)
	}
	if err := os.WriteFile(path, []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	llm, err := p.Model(context.Background(), "")
	if err != nil {
		t.Fatalf("Model after the file changed: %v", err)
	}
	if got := drain(t, llm, &adkmodel.LLMRequest{})[0].Content.Parts[0].Text; got != "first" {
		t.Errorf("replayed %q after the file changed, want %q", got, "first")
	}
}

func TestNewScriptedPerCall_CarriesStrict(t *testing.T) {
	t.Parallel()
	p, err := NewScriptedPerCall(writeScript(t, twoTurnScript), true)
	if err != nil {
		t.Fatalf("NewScriptedPerCall: %v", err)
	}
	llm, err := p.Model(context.Background(), "")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	// The recorded request has no Contents; this one does.
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{textContent(genai.RoleUser, "unrecorded")}}
	for _, err := range llm.GenerateContent(context.Background(), req, false) {
		if err == nil {
			t.Fatal("expected a strict mismatch")
		}
		if !strings.Contains(err.Error(), "strict mismatch") {
			t.Errorf("error %q missing 'strict mismatch'", err.Error())
		}
		return
	}
	t.Fatal("expected an iteration with an error")
}

func TestNewScriptedPerCall_ProviderNameIsScripted(t *testing.T) {
	t.Parallel()
	p, err := NewScriptedPerCall(writeScript(t, twoTurnScript), false)
	if err != nil {
		t.Fatalf("NewScriptedPerCall: %v", err)
	}
	if p.Name() != ProviderScripted {
		t.Errorf("Name() = %q, want %q", p.Name(), ProviderScripted)
	}
}
