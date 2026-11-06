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

// Package statestore is the home of the pluggable session-state
// backends described in docs/external-state-design.md (#1192). The
// first seam is the transcript: eventlog.Transcript owns a session's
// ordered event record, and a Handle built by eventlog.OpenTranscript
// reads it while keeping control, index and per-session state on the
// local SQL database.
//
// Backends live in subpackages (memory, and the SQL one inside
// pkg/eventlog itself as OpenSQLTranscript); each runs the suite in
// the conformance subpackage, which is the contract between them.
//
// The interface is declared in pkg/eventlog, which already owns Entry
// and QueryOption, so that a backend can import eventlog without a
// cycle; the alias here gives it the name the design uses.
package statestore

import "github.com/go-steer/core-agent/v3/pkg/eventlog"

// Transcript is eventlog.Transcript under the design's name.
type Transcript = eventlog.Transcript

// SessionKey identifies one session: the ADK (app, user, id) triple.
type SessionKey struct {
	AppName, UserID, SessionID string
}

// Root returns the key of the root session of k's tree (eventlog.RootOf).
func (k SessionKey) Root() SessionKey {
	return SessionKey{AppName: k.AppName, UserID: k.UserID, SessionID: eventlog.RootOf(k.SessionID)}
}
