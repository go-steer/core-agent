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

package config

import "fmt"

// callPeerMaxTimeoutSeconds is the ceiling on tools.call_peer.timeout_seconds.
// Mirrors peer.MaxTimeout in pkg/tools/peer, which cannot be imported
// here (that package imports this one). Kept as a plain number rather
// than a shared constant so pkg/config stays dependency-free.
const callPeerMaxTimeoutSeconds = 900

// validateCallPeer checks the tools.call_peer block. Structural only,
// per Validate's contract: whether token_env is actually set in the
// environment, and whether this daemon is a peer hub at all, are
// checked where that's knowable (the tool at call time, cmd/core-agent
// at startup).
//
// The block is validated even when Enabled is false. A typo'd knob
// that only surfaces the day someone flips enabled=true is a worse
// trade than failing now.
func (c *Config) validateCallPeer() error {
	cp := c.Tools.CallPeer
	if cp.Name != "" && !validToolName(cp.Name) {
		return fmt.Errorf("config: tools.call_peer.name=%q must be [A-Za-z_][A-Za-z0-9_]{0,63} (it becomes a model-facing function name, which rules out hyphens and dots)", cp.Name)
	}
	if cp.TimeoutSeconds < 0 {
		return fmt.Errorf("config: tools.call_peer.timeout_seconds=%d must not be negative (omit the field for the 120s default)", cp.TimeoutSeconds)
	}
	if cp.TimeoutSeconds > callPeerMaxTimeoutSeconds {
		return fmt.Errorf("config: tools.call_peer.timeout_seconds=%d exceeds the %ds ceiling (a delegated call that outlives a liveness probe wedges the caller)", cp.TimeoutSeconds, callPeerMaxTimeoutSeconds)
	}
	if cp.MaxResponseBytes < 0 {
		return fmt.Errorf("config: tools.call_peer.max_response_bytes=%d must not be negative (omit the field for the 16384 default)", cp.MaxResponseBytes)
	}
	return nil
}

// validToolName reports whether s is usable as a model-facing tool
// name. Stricter than validAlertName: this string is a function name
// in the model's schema, where the providers accept letters, digits,
// and underscores only, and won't take a leading digit.
func validToolName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
