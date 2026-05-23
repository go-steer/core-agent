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

import (
	"github.com/go-steer/core-agent/attach"
)

// Bubble tea message types live here so each model file can stay
// focused on its View/Update logic. All tea.Msg implementations are
// value types so they can be cheaply dispatched across cmd boundaries.

// --- Picker ---

type pickerSessionsLoadedMsg struct {
	sessions []pickerEntry
	err      error
}

// --- Chat / streaming ---

type chatFrameMsg struct {
	frame attach.Frame
}

type chatStreamEndedMsg struct {
	err error
}

type chatStatusLoadedMsg struct {
	status attach.StatusInfo
	err    error
}

type chatInjectAckMsg struct {
	message string
	err     error
}

type chatWakeAckMsg struct {
	err error
}

type chatToolsLoadedMsg struct {
	tools []attach.ToolInfo
	err   error
}

type chatAgentsLoadedMsg struct {
	agents []attach.AgentInfo
	err    error
}

type chatPeersLoadedMsg struct {
	// nil err + nil peers means the listener doesn't run peer-registration.
	peers []peerEntry
	err   error
}

// chatPeersLoadedMsg uses peerEntry; declared in this file too.

// --- Picker entries ---

type pickerEntry struct {
	App         string
	User        string
	SessionID   string
	HasEventLog bool
	Endpoint    string // URL of the listener that owns this session (peer endpoint or "")
	Origin      string // "local" or peer name
}

// --- Peer entries (subset of attachclient.PeerDescriptor surfaced in modals) ---

type peerEntry struct {
	Name     string
	Endpoint string
	Labels   map[string]string
	RegID    string
}
