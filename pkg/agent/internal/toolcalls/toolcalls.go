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

// Package toolcalls records the tool calls an agent made, as the
// runtime observed them going past.
//
// It exists for the delegation boundary. A subagent hands its parent
// prose — `return_result`'s text, or its last assistant turn. Prose
// cannot be cited, and a parent that is graded on grounding its claims
// therefore re-issues the reads its child already made. Measured on the
// GKE drill's OOMKill scenario, whose follow-up asks for a value the
// child already established AND for the read behind it: 4 of 6 runs
// re-issued one, and 66% of the bytes the parent read after the handoff
// were bytes it already had. Afterwards, 0 of 11 (#1014).
//
// Do not read that as a corpus-wide rate. The drill's RBAC scenario
// asks instead whether the workload is healthy NOW, which no metadata
// can answer, and it re-reads at the same rate before and after — it
// should (#1034).
//
// The fix is not a bigger or better-compressed payload. What the parent
// lacks is PROVENANCE — "the child ran this, with these arguments, and
// it succeeded" — and provenance is metadata the runtime already sees:
// every FunctionCall part in the child's event stream. A [Recorder]
// collects them so a delegation result can carry what the child DID
// alongside what it concluded, at roughly a hundred bytes per call
// against the several thousand a re-read costs. The parent keeps its
// isolation, because provenance is not payload.
//
// The child is never asked to report on itself. A self-report is
// another claim in need of evidence; these are observations, and a
// subagent cannot understate, embellish or forget them.
package toolcalls

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/adk/session"
)

// DefaultMaxCalls bounds how many calls one [Recorder] keeps.
//
// A delegation result is read by a language model with a finite
// context, and the whole point of this package is to spend fewer tokens
// than the re-read it replaces; an unbounded list from a runaway
// subagent would invert that. 64 is comfortably above every archived
// drill run (the busiest child made 7 calls) and far below the point
// where the list costs more than it saves.
const DefaultMaxCalls = 64

// DefaultMaxArgBytes bounds the serialized size of one argument value.
//
// Arguments are identifying information — a resource name, a namespace,
// a path — and at that size they cost almost nothing. But the same
// field carries a `bash` command line or a file's whole contents, and
// echoing those back to the parent would reintroduce the payload this
// package exists to avoid.
const DefaultMaxArgBytes = 256

// truncationMarker is appended to a value this package shortened. It
// names the package so a reader who finds one in a transcript can tell
// a truncated argument from a tool that genuinely returned an ellipsis.
const truncationMarker = "…[truncated by toolcalls]"

// Call is one tool invocation, as observed rather than as reported.
type Call struct {
	// Tool is the tool's registered name.
	Tool string `json:"tool"`
	// Args are the call's arguments, with oversized values shortened
	// (see [DefaultMaxArgBytes]). Presentational and identifying
	// arguments alike are kept: this is a record of what happened, not
	// a normalized key for comparing two calls.
	Args map[string]any `json:"args,omitempty"`
	// Error is the tool's failure text, empty for a call that returned
	// cleanly.
	//
	// It travels with the call because a citation to a call that failed
	// is worse than no citation: it reads as grounding while grounding
	// nothing. The 2026-09-11 drill run is the case — a delegation died
	// on a 429, and a `calls` list that recorded the attempt without
	// recording the failure would have invited the parent to cite it.
	Error string `json:"error,omitempty"`
}

// Recorder accumulates the tool calls in an event stream.
//
// The zero value is usable and applies the package defaults. Not safe
// for concurrent use — one Recorder belongs to one agent's event loop,
// which is single-threaded by construction.
type Recorder struct {
	// Skip lists tool names to leave out, compared case-insensitively.
	//
	// The caller's own control plane belongs here. In particular the
	// done/return tool: its argument IS the delegation's `output`
	// field, so recording it would ship the same prose twice in one
	// result — the opposite of the saving this package is for.
	Skip []string
	// MaxCalls overrides [DefaultMaxCalls]. Negative means unbounded.
	MaxCalls int
	// MaxArgBytes overrides [DefaultMaxArgBytes]. Negative means
	// values are never shortened.
	MaxArgBytes int

	calls   []Call
	byID    map[string]int
	dropped int
}

// Observe records any tool calls and tool outcomes carried by ev.
//
// Partial events are ignored. A streaming provider emits a function
// call as chunks and then again as one consolidated part, so counting
// partials would report a single call several times — the same
// discipline the text collectors in pkg/agent and pkg/agent/autonomous
// follow, and for the same reason.
func (r *Recorder) Observe(ev *session.Event) {
	if r == nil || ev == nil || ev.Partial || ev.Content == nil {
		return
	}
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		if fc := p.FunctionCall; fc != nil {
			r.recordCall(fc.ID, fc.Name, fc.Args)
		}
		// Not an else: ADK is free to put both in one event, and a
		// response is matched by ID rather than by position, so the
		// order the two arrive in does not matter.
		if fr := p.FunctionResponse; fr != nil {
			r.recordResponse(fr.ID, fr.Response)
		}
	}
}

func (r *Recorder) recordCall(id, name string, args map[string]any) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	for _, s := range r.Skip {
		if strings.EqualFold(s, name) {
			return
		}
	}
	if max := r.maxCalls(); max >= 0 && len(r.calls) >= max {
		// Counted, not silently dropped. Truncating a provenance
		// record without saying so would make the list quietly
		// unreliable, which is the failure this package exists to fix
		// rather than to reproduce.
		r.dropped++
		return
	}
	r.calls = append(r.calls, Call{Tool: name, Args: r.trimArgs(args)})
	if id != "" {
		if r.byID == nil {
			r.byID = make(map[string]int)
		}
		r.byID[id] = len(r.calls) - 1
	}
}

func (r *Recorder) recordResponse(id string, resp map[string]any) {
	if id == "" {
		return
	}
	i, ok := r.byID[id]
	if !ok {
		// Either the call was skipped, dropped past the cap, or
		// belongs to an earlier turn whose recorder is gone. Nothing
		// to attach the outcome to.
		return
	}
	r.calls[i].Error = ResponseError(resp)
}

// trimArgs copies args, shortening any value whose serialized form is
// over the limit. A copy because the map belongs to the event, which
// other observers on the same stream are still reading.
func (r *Recorder) trimArgs(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	limit := r.maxArgBytes()
	out := make(map[string]any, len(args))
	for k, v := range args {
		if limit < 0 {
			out[k] = v
			continue
		}
		raw, err := json.Marshal(v)
		if err != nil {
			// Unmarshalable values are rare and their %v form is
			// bounded by the same limit, so this keeps the argument
			// NAME visible rather than dropping the pair. Which
			// arguments a call carried is itself provenance.
			out[k] = truncate(fmt.Sprintf("%v", v), limit)
			continue
		}
		if len(raw) <= limit {
			out[k] = v
			continue
		}
		// Deliberately the string form and not a shortened copy of the
		// original type: a truncated JSON object is invalid JSON, and a
		// half-parsed argument is more misleading than an obviously
		// clipped string.
		out[k] = truncate(string(raw), limit)
	}
	return out
}

func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	// Cut on a rune boundary so the result stays valid UTF-8; a
	// mangled tail in an audit record reads as corruption.
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncationMarker
}

// Append adds src to dst, stopping at max total calls and reporting
// how many it refused. A negative max is unbounded.
//
// It exists because a per-recorder cap bounds one turn, and the thing
// that needs bounding is the delegation RESULT — a standing worker runs
// for hundreds of turns, each individually under the cap.
func Append(dst, src []Call, max int) (out []Call, dropped int) {
	if max < 0 || len(dst)+len(src) <= max {
		return append(dst, src...), 0
	}
	room := max - len(dst)
	if room < 0 {
		// dst is already over the cap. Unreachable from the run loop,
		// where the same max is applied every time, but a caller that
		// lowers max between calls or seeds a longer dst would slice
		// out of range and take the agent down — a bounds panic in a
		// bookkeeping helper is a bad way to end a delegation.
		room = 0
	}
	return append(dst, src[:room]...), len(src) - room
}

// Note is the prose that ships beside a non-empty call list: n
// recorded, truncated dropped. Empty when there is nothing to describe.
//
// A bare array does not change what a model does with it. That lesson
// is #710's — spawn_agent's stop_reason enum lost the argument to a
// well-formed `output` that read like a finished report, and what won
// it back was a sentence written in language, next to the data, at the
// moment of the decision. Shipping the calls without this would be
// shipping the enum without the sentence.
//
// It licenses the re-read that IS warranted. A parent re-running a call
// to learn whether state has since changed is doing its job; a parent
// re-running one to obtain something it is already entitled to cite is
// paying twice for one fact, and only the second is what #1014
// measures. Blurring the two would trade a redundancy problem for an
// ungroundedness problem.
//
// Lives here because both delegation doors need the identical sentence
// and the packages behind them (pkg/agent, pkg/agent/background) cannot
// import each other — two copies would drift, and a note that says
// something subtly different depending on which door a recipe wired is
// worse than no note.
func Note(n, truncated int) string {
	if n <= 0 {
		return ""
	}
	note := fmt.Sprintf("The %d call(s) above are what the subagent actually ran, recorded by the runtime rather than reported by the subagent. "+
		"Cite them as evidence for what was observed. "+
		"Re-run one only if you need its CURRENT value; re-running it to obtain a citation you already have is redundant.", n)
	if truncated > 0 {
		note += fmt.Sprintf(" %d further call(s) were made but not recorded — absence from this list does not mean it did not happen.", truncated)
	}
	return note
}

// Calls returns the recorded calls in the order they were observed.
func (r *Recorder) Calls() []Call {
	if r == nil {
		return nil
	}
	return r.calls
}

// Dropped returns how many calls were seen past [Recorder.MaxCalls] and
// therefore not recorded.
func (r *Recorder) Dropped() int {
	if r == nil {
		return 0
	}
	return r.dropped
}

func (r *Recorder) maxCalls() int {
	if r.MaxCalls == 0 {
		return DefaultMaxCalls
	}
	return r.MaxCalls
}

func (r *Recorder) maxArgBytes() int {
	if r.MaxArgBytes == 0 {
		return DefaultMaxArgBytes
	}
	return r.MaxArgBytes
}

// ResponseError extracts the tool error from an ADK function response,
// returning "" for a successful call. The reserved key is "error".
//
// A non-string, non-error value under "error" still counts as a
// failure — a tool that returns a structured error object is failing,
// and treating an unrecognized shape as success would silently drop
// exactly the observations this signal exists to make.
func ResponseError(resp map[string]any) string {
	v, ok := resp["error"]
	if !ok || v == nil {
		return ""
	}
	switch e := v.(type) {
	case string:
		if e == "" {
			return ""
		}
		return e
	case error:
		return e.Error()
	default:
		return fmt.Sprintf("%v", e)
	}
}
