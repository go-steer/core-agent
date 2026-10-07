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

import (
	"slices"
	"testing"
)

// The bearer table is protected whenever it is configured — switched
// off or not, since it still holds live tokens — and the TLS key, which
// authenticates the daemon rather than a caller, is not.
func TestCredentialFiles(t *testing.T) {
	t.Parallel()
	var nilCfg *Config
	if got := nilCfg.CredentialFiles(); got != nil {
		t.Errorf("nil config: %v", got)
	}
	c := DefaultConfig()
	if got := c.CredentialFiles(); len(got) != 0 {
		t.Errorf("default config names credential files: %v", got)
	}
	c.Attach.TLSKey = "/etc/tls/key.pem"
	c.Attach.MultiSession.Auth.TableFile = "/etc/core-agent/users.json"
	if got, want := c.CredentialFiles(), []string{"/etc/core-agent/users.json"}; !slices.Equal(got, want) {
		t.Errorf("CredentialFiles() = %v, want %v", got, want)
	}
	c.Attach.MultiSession.Enabled = true
	if got := c.CredentialFiles(); len(got) != 1 {
		t.Errorf("enabling multi-session changed the set: %v", got)
	}
}
