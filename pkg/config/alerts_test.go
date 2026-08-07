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
	"strings"
	"testing"
	"time"
)

func TestValidateAlerts_Valid(t *testing.T) {
	t.Parallel()
	cases := map[string]AlertsConfig{
		"empty": {},
		"literal url + generic": {Targets: []AlertTarget{
			{Name: "audit", URL: "https://example.com/hook", Template: AlertTemplateGeneric},
		}},
		"url_env": {Targets: []AlertTarget{
			{Name: "slack", URLEnv: "SLACK_WEBHOOK_URL", Template: AlertTemplateGeneric},
		}},
		"explicit webhook kind": {Targets: []AlertTarget{
			{Name: "audit", Kind: "webhook", URL: "https://example.com/hook", Template: AlertTemplateGeneric},
		}},
		"bearer auth": {Targets: []AlertTarget{
			{Name: "pd", URL: "https://events.pagerduty.com/v2/enqueue", Template: AlertTemplateGeneric, Auth: &AlertAuth{BearerEnv: "PD_TOKEN"}},
		}},
		"basic auth": {Targets: []AlertTarget{
			{Name: "legacy", URL: "https://example.com/hook", Template: AlertTemplateGeneric, Auth: &AlertAuth{BasicEnvUser: "U", BasicEnvPass: "P"}},
		}},
		"multiple distinct names": {Targets: []AlertTarget{
			{Name: "a", URL: "https://a.example.com", Template: AlertTemplateGeneric},
			{Name: "b", URL: "https://b.example.com", Template: AlertTemplateGeneric},
		}},
		"rate limit N/window": {
			Targets:            []AlertTarget{{Name: "a", URL: "https://a.example.com", Template: AlertTemplateGeneric}},
			RateLimitPerTarget: "5/min",
		},
		"rate limit bare duration": {
			Targets:            []AlertTarget{{Name: "a", URL: "https://a.example.com", Template: AlertTemplateGeneric}},
			RateLimitPerTarget: "30s",
		},
	}
	for name, a := range cases {
		a := a
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &Config{Alerts: a}
			if err := c.validateAlerts(); err != nil {
				t.Errorf("validateAlerts() = %v, want nil", err)
			}
		})
	}
}

func TestValidateAlerts_Invalid(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		alerts  AlertsConfig
		wantSub string
	}{
		"missing name": {
			AlertsConfig{Targets: []AlertTarget{{URL: "https://x", Template: AlertTemplateGeneric}}},
			"name is required",
		},
		"bad name char": {
			AlertsConfig{Targets: []AlertTarget{{Name: "bad name", URL: "https://x", Template: AlertTemplateGeneric}}},
			"[A-Za-z0-9_-]",
		},
		"name too long": {
			AlertsConfig{Targets: []AlertTarget{{Name: strings.Repeat("a", 65), URL: "https://x", Template: AlertTemplateGeneric}}},
			"[A-Za-z0-9_-]",
		},
		"duplicate name": {
			AlertsConfig{Targets: []AlertTarget{
				{Name: "dup", URL: "https://a", Template: AlertTemplateGeneric},
				{Name: "dup", URL: "https://b", Template: AlertTemplateGeneric},
			}},
			"duplicate name",
		},
		"both url and url_env": {
			AlertsConfig{Targets: []AlertTarget{{Name: "a", URL: "https://x", URLEnv: "X", Template: AlertTemplateGeneric}}},
			"exactly one of url or url_env",
		},
		"neither url nor url_env": {
			AlertsConfig{Targets: []AlertTarget{{Name: "a", Template: AlertTemplateGeneric}}},
			"exactly one of url or url_env",
		},
		"bad url scheme": {
			AlertsConfig{Targets: []AlertTarget{{Name: "a", URL: "ftp://x", Template: AlertTemplateGeneric}}},
			"must be http(s)",
		},
		"url no host": {
			AlertsConfig{Targets: []AlertTarget{{Name: "a", URL: "https://", Template: AlertTemplateGeneric}}},
			"no host",
		},
		"unsupported kind": {
			AlertsConfig{Targets: []AlertTarget{{Name: "a", Kind: "smtp", URL: "https://x", Template: AlertTemplateGeneric}}},
			"kind=\"smtp\"",
		},
		"template missing": {
			AlertsConfig{Targets: []AlertTarget{{Name: "a", URL: "https://x"}}},
			"template is required",
		},
		"template not yet implemented": {
			AlertsConfig{Targets: []AlertTarget{{Name: "a", URL: "https://x", Template: AlertTemplateSlack}}},
			"not yet implemented",
		},
		"template unknown": {
			AlertsConfig{Targets: []AlertTarget{{Name: "a", URL: "https://x", Template: "carrier-pigeon"}}},
			"is unknown",
		},
		"auth both schemes": {
			AlertsConfig{Targets: []AlertTarget{{Name: "a", URL: "https://x", Template: AlertTemplateGeneric, Auth: &AlertAuth{BearerEnv: "B", BasicEnvUser: "U", BasicEnvPass: "P"}}}},
			"not both",
		},
		"auth basic missing pass": {
			AlertsConfig{Targets: []AlertTarget{{Name: "a", URL: "https://x", Template: AlertTemplateGeneric, Auth: &AlertAuth{BasicEnvUser: "U"}}}},
			"BOTH basic_env_user and basic_env_pass",
		},
		"auth empty block": {
			AlertsConfig{Targets: []AlertTarget{{Name: "a", URL: "https://x", Template: AlertTemplateGeneric, Auth: &AlertAuth{}}}},
			"auth is set but empty",
		},
		"bad rate limit": {
			AlertsConfig{
				Targets:            []AlertTarget{{Name: "a", URL: "https://x", Template: AlertTemplateGeneric}},
				RateLimitPerTarget: "banana",
			},
			"rate_limit_per_target",
		},
	}
	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &Config{Alerts: tc.alerts}
			err := c.validateAlerts()
			if err == nil {
				t.Fatalf("validateAlerts() = nil, want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("validateAlerts() = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestParseAlertRateLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in         string
		wantCount  int
		wantWindow time.Duration
	}{
		{"30s", 1, 30 * time.Second},
		{"1/30s", 1, 30 * time.Second},
		{"5/min", 5, time.Minute},
		{"100/hour", 100, time.Hour},
		{"2/day", 2, 24 * time.Hour},
		{"1m30s", 1, 90 * time.Second},
		{" 5 / min ", 5, time.Minute}, // tolerant of surrounding whitespace
		{"1/h", 1, time.Hour},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			count, window, err := ParseAlertRateLimit(tc.in)
			if err != nil {
				t.Fatalf("ParseAlertRateLimit(%q) = %v", tc.in, err)
			}
			if count != tc.wantCount || window != tc.wantWindow {
				t.Errorf("ParseAlertRateLimit(%q) = (%d, %s), want (%d, %s)", tc.in, count, window, tc.wantCount, tc.wantWindow)
			}
		})
	}
}

func TestParseAlertRateLimit_Errors(t *testing.T) {
	t.Parallel()
	bad := []string{"", "banana", "0/min", "-1/30s", "x/min", "5/", "5/banana", "1/0s"}
	for _, in := range bad {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if _, _, err := ParseAlertRateLimit(in); err == nil {
				t.Errorf("ParseAlertRateLimit(%q) = nil error, want error", in)
			}
		})
	}
}
