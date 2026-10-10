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

package models

import (
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// A profile name with pkg/models/profiles not linked in says which
// import is missing, rather than calling the name unknown.
func TestResolve_ProfileWithoutProfilesPackage(t *testing.T) {
	prev := profileConstructor
	profileConstructor = nil
	t.Cleanup(func() { profileConstructor = prev })

	cfg := config.DefaultConfig()
	cfg.Model.Provider = "vllm"
	_, err := Resolve(cfg)
	if err == nil || !strings.Contains(err.Error(), "pkg/models/profiles") {
		t.Fatalf("Resolve err = %v, want the missing import named", err)
	}
}

// A profile name reaches the registered profile constructor; an unknown
// name lists the profiles alongside the registered providers.
func TestResolve_RoutesProfilesAndListsThem(t *testing.T) {
	prev := profileConstructor
	t.Cleanup(func() { profileConstructor = prev })
	var got string
	RegisterProfiles(func(c *config.Config) (Provider, error) {
		got = c.Model.Provider
		return nil, nil
	})

	cfg := config.DefaultConfig()
	cfg.Model.Provider = "vertex-maas"
	if _, err := Resolve(cfg); err != nil || got != "vertex-maas" {
		t.Fatalf("Resolve(vertex-maas) = %v, constructor saw %q", err, got)
	}
	cfg.Model.Provider = "nope"
	if _, err := Resolve(cfg); err == nil || !strings.Contains(err.Error(), "vertex-maas") {
		t.Fatalf("Resolve(nope) err = %v, want profiles listed", err)
	}
}
