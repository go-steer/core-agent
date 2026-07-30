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

package agent

import (
	"fmt"

	"google.golang.org/genai"
)

// normalizeToolPairs makes a raw event-content slice safe to send as
// model contents (#541). The internal history builders
// (summarizerHistory, sessionHistory) feed event contents verbatim,
// with none of the call/response rearrangement ADK's contents
// processor applies on the main turn path — but providers enforce the
// same two invariants everywhere:
//
//   - every functionCall must be answered, and
//   - its functionResponse must follow immediately (the #537 tail
//     repair appends its synthesized response at the history tail,
//     arbitrarily far from the call it answers).
//
// Normalization:
//
//   - a content holding only functionResponse parts is withheld from
//     its original position and re-emitted directly after the content
//     holding its matching functionCall;
//   - functionCall parts with no response anywhere are stripped (the
//     surrounding text parts survive);
//   - functionResponse parts whose call appears nowhere are dropped
//     at part level (surrounding text parts survive here too);
//   - contents left with zero parts are dropped.
//
// Pairing is by call ID when present. Empty-ID calls and responses —
// a real shape in replayed Gemini-origin histories (#367) — pair
// positionally by tool name + instance order, mirroring the Anthropic
// converter's name/order fallback, so `call("") resp("") call("")
// resp("")` stays two pairs instead of collapsing into one.
//
// Contents are shallow-copied when parts are filtered; the originals
// — which alias live event contents — are never mutated. Fully
// paired, adjacent histories come out pointer-identical.
//
// Known limitation: duplicate non-empty call IDs (impossible in
// committed ADK histories) pair as a set, so extra instances go
// unanswered.
func normalizeToolPairs(contents []*genai.Content) []*genai.Content {
	// Pass 1: assign each tool part a pairing key. Non-empty IDs key
	// as "id:<ID>". Empty IDs key as "nk:<name>#<instance>", counting
	// call and response instances independently in content order so
	// the k-th anonymous call of a tool pairs with the k-th anonymous
	// response of the same tool.
	callSeq := map[string]int{}
	respSeq := map[string]int{}
	keyFor := func(id, name string, seq map[string]int) string {
		if id != "" {
			return "id:" + id
		}
		k := fmt.Sprintf("nk:%s#%d", name, seq[name])
		seq[name]++
		return k
	}
	partKeys := make([][]string, len(contents))
	callKeys := map[string]bool{}
	respLoc := map[string][]int{} // key → content indices holding the response
	for i, c := range contents {
		if c == nil {
			continue
		}
		partKeys[i] = make([]string, len(c.Parts))
		for j, p := range c.Parts {
			switch {
			case p == nil:
			case p.FunctionCall != nil:
				k := keyFor(p.FunctionCall.ID, p.FunctionCall.Name, callSeq)
				partKeys[i][j] = k
				callKeys[k] = true
			case p.FunctionResponse != nil:
				k := keyFor(p.FunctionResponse.ID, p.FunctionResponse.Name, respSeq)
				partKeys[i][j] = k
				respLoc[k] = append(respLoc[k], i)
			}
		}
	}
	answered := func(key string) bool { return len(respLoc[key]) > 0 }

	// filter returns content i with unanswered calls and orphaned
	// responses stripped, plus the kept call keys (in part order) and
	// whether any response part survived. Shallow-copies only when a
	// part is actually dropped.
	filter := func(i int) (kept *genai.Content, keptCalls []string, hasResp bool) {
		c := contents[i]
		drop := func(j int) bool {
			p := c.Parts[j]
			if p == nil {
				return false
			}
			if p.FunctionCall != nil && !answered(partKeys[i][j]) {
				return true
			}
			if p.FunctionResponse != nil && !callKeys[partKeys[i][j]] {
				return true
			}
			return false
		}
		anyDrop := false
		for j := range c.Parts {
			if drop(j) {
				anyDrop = true
				break
			}
		}
		kept = c
		if anyDrop {
			cc := *c
			cc.Parts = nil
			for j, p := range c.Parts {
				if drop(j) {
					continue
				}
				cc.Parts = append(cc.Parts, p)
			}
			kept = &cc
		}
		for j, p := range c.Parts {
			if p == nil || drop(j) {
				continue
			}
			if p.FunctionCall != nil {
				keptCalls = append(keptCalls, partKeys[i][j])
			}
			if p.FunctionResponse != nil {
				hasResp = true
			}
		}
		return kept, keptCalls, hasResp
	}

	out := make([]*genai.Content, 0, len(contents))
	emitted := make([]bool, len(contents))
	// emit appends content i's filtered form and pulls each of its
	// kept calls' response contents in right behind it, iteratively
	// (a pulled content may itself carry calls — degenerate mixed
	// shapes — whose responses are then pulled in turn).
	emit := func(i int) {
		work := []int{i}
		for len(work) > 0 {
			idx := work[0]
			work = work[1:]
			if emitted[idx] {
				continue
			}
			emitted[idx] = true
			kept, keptCalls, _ := filter(idx)
			if len(kept.Parts) > 0 {
				out = append(out, kept)
			}
			for _, k := range keptCalls {
				for _, ri := range respLoc[k] {
					if !emitted[ri] {
						work = append(work, ri)
					}
				}
			}
		}
	}
	for i, c := range contents {
		if c == nil || emitted[i] {
			continue
		}
		_, keptCalls, hasResp := filter(i)
		if hasResp && len(keptCalls) == 0 {
			// Response-bearing content with no calls of its own: it is
			// emitted behind its matching call, never from here. (Its
			// call always exists — a kept response implies an answered,
			// kept call — so it cannot be stranded.)
			continue
		}
		emit(i)
	}
	return out
}
