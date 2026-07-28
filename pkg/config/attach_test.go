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
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAttachConfig_RoundTrip(t *testing.T) {
	t.Parallel()

	body := `{
		"version": 1,
		"model": { "name": "claude-opus-4-7" },
		"attach": {
			"listen":            "0.0.0.0:7777",
			"tls_cert":          "/etc/attach/tls.crt",
			"tls_key":           "/etc/attach/tls.key",
			"client_ca":         "/etc/attach/ca.crt",
			"token_env":         "ATTACH_TOKEN",
			"readonly":          true,
			"peer_hub":          true,
			"register_to":       "https://hub.svc:7777",
			"register_endpoint": "https://${POD_IP}:7777",
			"register_name":     "monitor-${HOSTNAME}"
		}
	}`

	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := cfg.Attach
	want := AttachConfig{
		Listen:           "0.0.0.0:7777",
		TLSCert:          "/etc/attach/tls.crt",
		TLSKey:           "/etc/attach/tls.key",
		ClientCA:         "/etc/attach/ca.crt",
		TokenEnv:         "ATTACH_TOKEN",
		ReadOnly:         true,
		PeerHub:          true,
		RegisterTo:       "https://hub.svc:7777",
		RegisterEndpoint: "https://${POD_IP}:7777", // unexpanded at load time
		RegisterName:     "monitor-${HOSTNAME}",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AttachConfig round-trip mismatch:\n got:  %+v\n want: %+v", got, want)
	}
}

func TestAttachConfig_OmittedSection(t *testing.T) {
	t.Parallel()

	// A config without an "attach" key should leave Attach as the zero value.
	body := `{ "version": 1, "model": { "name": "claude-opus-4-7" } }`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg.Attach, AttachConfig{}) {
		t.Errorf("Attach should be zero value when absent, got: %+v", cfg.Attach)
	}
}

func TestAttachConfig_CostRateLimit(t *testing.T) {
	t.Parallel()

	// Round-trip: the block parses and lands on the struct.
	body := `{
		"version": 1,
		"model": { "name": "claude-opus-4-7" },
		"attach": { "cost_rate_limit": { "per_minute": 30, "burst": 10 } }
	}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rl := cfg.Attach.CostRateLimit
	if rl == nil || rl.PerMinute != 30 || rl.Burst != 10 || rl.Disabled {
		t.Fatalf("CostRateLimit = %+v, want {30 10 false}", rl)
	}

	// Omitted → nil (library defaults apply downstream).
	cfg2 := DefaultConfig()
	if cfg2.Attach.CostRateLimit != nil {
		t.Errorf("default CostRateLimit = %+v, want nil", cfg2.Attach.CostRateLimit)
	}

	// Validation: negatives rejected; zero + disabled accepted.
	for _, tc := range []struct {
		rl      *CostRateLimitConfig
		wantErr bool
	}{
		{&CostRateLimitConfig{PerMinute: -1}, true},
		{&CostRateLimitConfig{Burst: -5}, true},
		{&CostRateLimitConfig{}, false},
		{&CostRateLimitConfig{Disabled: true}, false},
		{&CostRateLimitConfig{PerMinute: 120, Burst: 20}, false},
	} {
		c := DefaultConfig()
		c.Attach.CostRateLimit = tc.rl
		err := c.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("Validate(%+v): want error, got nil", tc.rl)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("Validate(%+v): unexpected error: %v", tc.rl, err)
		}
	}
}
