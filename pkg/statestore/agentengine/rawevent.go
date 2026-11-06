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

package agentengine

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/adk/v2/session"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

// Agent Engine session IDs must start with a letter, end with a letter
// or digit, and be at most 63 characters of [a-z0-9-] (decision 8).
// Ours often do not: UUIDv7 IDs can start with a digit, and derived IDs
// carry ":".
var nativeIDPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// mapID returns the Agent Engine session ID for one of ours. An ID the
// service accepts passes unchanged; any other maps deterministically to
// "s-" + the first 40 characters of lower-case base32(sha256(id)), 42
// characters of [a-z2-7-]. The original is kept in session state
// (stateKeySessionID) so nothing above the store layer sees the mapped
// form.
func mapID(id string) string {
	if nativeIDPattern.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	enc := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))
	return "s-" + enc[:40]
}

// Session state keys and the label the backend writes. State carries
// the identity Agent Engine cannot (our ID, the root's ID, the user);
// the label lets a tree be found with one ListSessions filter where
// label filters are supported, and client-side otherwise.
const (
	stateKeyAppName   = "core_agent.app_name"
	stateKeySessionID = "core_agent.session_id"
	stateKeyRootID    = "core_agent.root_id"
	labelRoot         = "core-agent-root"
)

// rawEventVersion is the schema version of the coreAgent block.
const rawEventVersion = 1

// rawEventKey is the one key this backend adds to the event's own JSON
// inside SessionEvent.raw_event. The event is written at the top level
// so another ADK runtime reading raw_event as an Event still gets the
// event (unknown keys are ignored by Go's and Python's decoders); the
// seq and sidecar the overlay table used to carry ride under this key
// (decision 6).
const rawEventKey = "coreAgent"

// rawEventMeta is the coreAgent block.
type rawEventMeta struct {
	V        int               `json:"v"`
	Seq      int64             `json:"seq"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func encodeRawEvent(seq int64, md map[string]string, ev *session.Event) (*structpb.Struct, error) {
	evJSON, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("agentengine: encode event %q: %w", ev.ID, err)
	}
	var m map[string]any
	if err := json.Unmarshal(evJSON, &m); err != nil {
		return nil, fmt.Errorf("agentengine: event %q is not a JSON object: %w", ev.ID, err)
	}
	metaJSON, err := json.Marshal(rawEventMeta{V: rawEventVersion, Seq: seq, Metadata: md})
	if err != nil {
		return nil, fmt.Errorf("agentengine: encode raw_event meta for %q: %w", ev.ID, err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		return nil, fmt.Errorf("agentengine: raw_event meta for %q: %w", ev.ID, err)
	}
	m[rawEventKey] = meta
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil, fmt.Errorf("agentengine: raw_event for %q: %w", ev.ID, err)
	}
	return s, nil
}

// decodeRawEvent returns the seq, sidecar and event from a raw_event
// written by encodeRawEvent. A SessionEvent without the coreAgent block
// (written by something other than this backend) is not ours and is
// skipped by the caller.
func decodeRawEvent(raw *structpb.Struct) (eventlog.Entry, error) {
	if raw == nil {
		return eventlog.Entry{}, errNotOurs
	}
	m := raw.AsMap()
	metaAny, ok := m[rawEventKey]
	if !ok {
		return eventlog.Entry{}, errNotOurs
	}
	delete(m, rawEventKey)
	metaJSON, err := json.Marshal(metaAny)
	if err != nil {
		return eventlog.Entry{}, fmt.Errorf("agentengine: raw_event meta: %w", err)
	}
	var meta rawEventMeta
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		return eventlog.Entry{}, fmt.Errorf("agentengine: raw_event meta: %w", err)
	}
	if meta.V != rawEventVersion || meta.Seq == 0 {
		return eventlog.Entry{}, errNotOurs
	}
	evJSON, err := json.Marshal(m)
	if err != nil {
		return eventlog.Entry{}, fmt.Errorf("agentengine: raw_event: %w", err)
	}
	ev := &session.Event{}
	if err := json.Unmarshal(evJSON, ev); err != nil {
		return eventlog.Entry{}, fmt.Errorf("agentengine: raw_event event: %w", err)
	}
	var md map[string]string
	if len(meta.Metadata) > 0 {
		md = meta.Metadata
	}
	return eventlog.Entry{Seq: meta.Seq, Event: ev, Metadata: md}, nil
}

// errNotOurs marks a SessionEvent this backend did not write.
var errNotOurs = fmt.Errorf("agentengine: event has no core-agent raw_event block")
