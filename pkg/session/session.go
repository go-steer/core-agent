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

// Package session is the deprecated former name of pkg/transcript
// (#492): ADK's own session package dominates pkg/agent's
// signatures, so this name collided in almost every file that used
// both. Everything here forwards; the on-disk layout
// (.agents/sessions/, schema v1) is unchanged.
//
// Deprecated: use pkg/transcript. Removed in v4.0.
package session

import "github.com/go-steer/core-agent/v3/pkg/transcript"

// SchemaVersion is the on-disk schema version for transcripts.
//
// Deprecated: use transcript.SchemaVersion. Removed in v4.0.
const SchemaVersion = transcript.SchemaVersion

// SessionsDirName is the directory under .agents/ that holds saved
// transcripts.
//
// Deprecated: use transcript.SessionsDirName. Removed in v4.0.
const SessionsDirName = transcript.SessionsDirName

// Transcript captures one session for archival.
//
// Deprecated: use transcript.Transcript. Removed in v4.0.
type Transcript = transcript.Transcript

// Message is one chat message in a Transcript.
//
// Deprecated: use transcript.Message. Removed in v4.0.
type Message = transcript.Message

// Usage is the final usage totals in a Transcript.
//
// Deprecated: use transcript.Usage. Removed in v4.0.
type Usage = transcript.Usage

// ErrNoProject reports that no project .agents/ directory is
// configured.
//
// Deprecated: use transcript.ErrNoProject. Removed in v4.0.
var ErrNoProject = transcript.ErrNoProject

// Save persists t under agentsDir.
//
// Deprecated: use transcript.Save. Removed in v4.0.
func Save(agentsDir string, t Transcript) (string, error) {
	return transcript.Save(agentsDir, t)
}
