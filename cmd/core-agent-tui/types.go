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

package main

// Bubble-tea message + entry types for the pre-coretui picker. The
// chat-side message types (frame / inject-ack / etc.) live with the
// adapter now (internal/coretuiremote) — this file is whatever the
// picker.go needs to compile.

// pickerSessionsLoadedMsg is dispatched by the picker's refresh
// command when the listener's GET /sessions response arrives.
type pickerSessionsLoadedMsg struct {
	sessions []pickerEntry
	err      error
}

// pickerSessionCreatedMsg is dispatched after the operator activates
// the "+ New session" row in the picker. err is set when the daemon
// returned 501 (no SessionFactory wired), 401 (no caller), or any
// other failure — the picker surfaces it without crashing.
type pickerSessionCreatedMsg struct {
	entry pickerEntry
	err   error
}

// entryKind tags a pickerEntry as either a real session row or the
// "+ New session" sentinel that triggers POST /sessions when the
// operator selects it. Default kindSession matches the historical
// shape so call sites that don't set kind keep working unchanged.
type entryKind int

const (
	kindSession entryKind = iota
	kindCreate
)

// pickerEntry is one row in the session picker. App may be empty
// when the listener didn't qualify the SessionID (the shortcut form
// works either way). Endpoint is the URL of the listener that owns
// this session — populated to disambiguate peer-registered sessions
// from local ones.
//
// Kind == kindCreate marks the synthetic "+ New session" sentinel
// row; the SessionID/App/User fields are ignored on creation rows.
type pickerEntry struct {
	Kind        entryKind
	App         string
	User        string
	SessionID   string
	HasEventLog bool
	Endpoint    string // listener URL (peer endpoint or "")
	Origin      string // "local" | peer name
}

// sessionPath returns the relative attach path the entry corresponds
// to. Used by main.go after the picker selects a session to construct
// the coretuiremote adapter.
func (e pickerEntry) sessionPath() string {
	if e.App == "" {
		return "/sessions/" + e.SessionID
	}
	return "/sessions/" + e.App + "/" + e.SessionID
}
