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

// CredentialFiles returns every file this config makes the daemon read
// a caller credential from — a credential that authenticates someone to
// the daemon and so can answer the agent's permission prompts (#1201).
// The permission gate refuses every agent tool call that names one.
//
// Today that is the multi-session bearer table. It is returned whenever
// it is set, enabled or not: a table the operator left configured but
// switched off still holds live tokens, and the first boot that turns
// multi-session on must not be the first boot that protects it.
//
// Unlike EnvRefs this is a hand-written list, because "is a file path"
// is not a property a field's name carries (system_prompt_file,
// peer_state_file and instructions_file are paths, not credentials).
// A new config field naming a file the daemon reads a caller credential
// from belongs here; a test pins the current set.
//
// Deliberately absent: attach.tls_key. It authenticates the daemon to
// its clients, not a caller to the daemon, so it answers no prompt.
// Provider and cloud credential files (ADC) are absent for the reason
// they are absent from EnvRefs' withheld set: SDKs consume them and a
// coding agent's own tooling needs them.
//
// A nil *Config returns nil.
func (c *Config) CredentialFiles() []string {
	if c == nil {
		return nil
	}
	var out []string
	if p := c.Attach.MultiSession.Auth.TableFile; p != "" {
		out = append(out, p)
	}
	return out
}
