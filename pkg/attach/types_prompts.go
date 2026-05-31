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

package attach

import "time"

// PromptFrame is one record on the /perms/stream SSE channel. Mirrors
// permissions.PromptRequest in a JSON-friendly shape plus the
// server-generated ID used to correlate the eventual
// /perms/respond. Kind uses the wire-stable string form rather than
// the in-process enum's int value so renumbering the enum doesn't
// break attached clients.
type PromptFrame struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"` // "bash" | "file_write" | "path_scope" | "generic"
	ToolName    string    `json:"tool"`
	Detail      string    `json:"detail,omitempty"`
	Verb        string    `json:"verb,omitempty"`
	Source      string    `json:"source,omitempty"`
	PersistTool string    `json:"persist_tool,omitempty"`
	PersistKey  string    `json:"persist_key,omitempty"`
	Access      string    `json:"access,omitempty"` // "" for non-path-scope prompts
	At          time.Time `json:"at"`
}

// PromptResponse is the POST body for /perms/respond. Decision uses
// the wire-stable string form; see DecisionFromWire for the mapping.
type PromptResponse struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
}
