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

package scionremote

import (
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"

	"github.com/go-steer/core-agent/agent"
)

// Classifier turns a Scion cloud-log entry into a RemoteAgentEvent
// (or drops it). Returning ok=false means "ignore this log line."
//
// Bundled classifiers:
//
//   - PreferStructuredPayload (default) — uses jsonPayload.kind +
//     jsonPayload.text when present; falls back to StringPrefix; if
//     neither matches, drops the entry.
//   - StringPrefix — looks for "[REPORT_ALERT]", "[REPORT_COMPLETED]",
//     "[REPORT_FAILED]" prefixes on the message text. Convention for
//     non-core-agent spawned agents.
//   - Verbose — emits every log entry as Kind="log" (no filtering).
//     Spammy; intended for debugging the integration.
type Classifier func(hubclient.CloudLogEntry) (agent.RemoteAgentEvent, bool)

// PreferStructuredPayload is the default classifier. Looks at the
// log entry's jsonPayload for a known structured shape, falls back
// to string-prefix parsing, and drops anything that isn't a
// recognisable report.
//
// Expected jsonPayload shape (emitted by core-agent's report_alert
// when it's running under the scion-agent harness's structured-log
// emitter):
//
//	{"kind": "alert"|"completed"|"failed", "text": "...", "from": "..."}
func PreferStructuredPayload(entry hubclient.CloudLogEntry) (agent.RemoteAgentEvent, bool) {
	if ev, ok := classifyJSONPayload(entry); ok {
		return ev, true
	}
	if ev, ok := StringPrefix(entry); ok {
		return ev, true
	}
	return agent.RemoteAgentEvent{}, false
}

// StringPrefix is a fallback classifier for spawned agents that
// don't emit structured payloads. Recognises message prefixes:
//
//	"[REPORT_ALERT] text..."     → Kind=alert
//	"[REPORT_COMPLETED] text..." → Kind=completed
//	"[REPORT_FAILED] text..."    → Kind=failed
//
// Any other message returns ok=false (dropped).
func StringPrefix(entry hubclient.CloudLogEntry) (agent.RemoteAgentEvent, bool) {
	msg := strings.TrimSpace(entry.Message)
	for _, p := range stringPrefixes {
		if strings.HasPrefix(msg, p.prefix) {
			return agent.RemoteAgentEvent{
				Kind:      p.kind,
				Text:      strings.TrimSpace(strings.TrimPrefix(msg, p.prefix)),
				Timestamp: entry.Timestamp,
			}, true
		}
	}
	return agent.RemoteAgentEvent{}, false
}

// Verbose surfaces every log entry as Kind="log". Useful when
// debugging the integration; not recommended for production.
func Verbose(entry hubclient.CloudLogEntry) (agent.RemoteAgentEvent, bool) {
	return agent.RemoteAgentEvent{
		Kind:      "log",
		Text:      entry.Message,
		Timestamp: entry.Timestamp,
	}, true
}

// stringPrefixes is the lookup table for StringPrefix. Order
// doesn't matter (no prefix is a prefix of another).
var stringPrefixes = []struct {
	prefix string
	kind   string
}{
	{"[REPORT_ALERT]", "alert"},
	{"[REPORT_COMPLETED]", "completed"},
	{"[REPORT_FAILED]", "failed"},
}

// classifyJSONPayload extracts a RemoteAgentEvent from the entry's
// jsonPayload when it carries the structured shape this package
// expects. ok=false when the payload is missing or doesn't have a
// recognised kind.
func classifyJSONPayload(entry hubclient.CloudLogEntry) (agent.RemoteAgentEvent, bool) {
	if entry.JSONPayload == nil {
		return agent.RemoteAgentEvent{}, false
	}
	kindV, ok := entry.JSONPayload["kind"]
	if !ok {
		return agent.RemoteAgentEvent{}, false
	}
	kind, ok := kindV.(string)
	if !ok || kind == "" {
		return agent.RemoteAgentEvent{}, false
	}
	switch kind {
	case "alert", "completed", "failed":
		// recognised
	default:
		return agent.RemoteAgentEvent{}, false
	}
	text := ""
	if v, ok := entry.JSONPayload["text"]; ok {
		if s, ok := v.(string); ok {
			text = s
		}
	}
	return agent.RemoteAgentEvent{
		Kind:      kind,
		Text:      text,
		Timestamp: entry.Timestamp,
	}, true
}
