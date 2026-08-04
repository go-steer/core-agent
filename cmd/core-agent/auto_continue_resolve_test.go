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
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

func boolPtr(b bool) *bool { return &b }

// TestResolveAutoContinue is the #559 gate: it locks in the
// precondition-gated default-on behavior. The rows marked "fails-first"
// are the ones that fail against pre-#559 code (where enabled was a plain
// bool defaulting to off and had no opt-out that survived a nil block).
func TestResolveAutoContinue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		ac           *config.AutoContinueConfig
		multiSession bool
		noREPL       bool
		haveEventlog bool
		wantEnabled  bool
		wantRetry    bool // only asserted when wantEnabled
		wantWarnSub  string
		wantErr      bool
	}{
		// --- unset (the default-on gate) ---
		{
			name:         "unset+multiSession+eventlog=on(default)",
			ac:           nil,
			multiSession: true,
			haveEventlog: true,
			wantEnabled:  true,
			wantRetry:    true,
			wantWarnSub:  "on by default",
		},
		{
			name:         "unset+noREPL+eventlog=on(default)",
			ac:           nil,
			noREPL:       true,
			haveEventlog: true,
			wantEnabled:  true,
			wantRetry:    true,
			wantWarnSub:  "on by default",
		},
		{
			name:         "unset+REPL+eventlog=off(silent)",
			ac:           nil,
			haveEventlog: true,
			wantEnabled:  false,
		},
		{
			name:         "unset+multiSession+noEventlog=off(silent)",
			ac:           nil,
			multiSession: true,
			haveEventlog: false,
			wantEnabled:  false,
		},
		// empty-but-present block behaves identically to nil (Enabled nil)
		{
			name:         "emptyBlock+multiSession+eventlog=on(default)",
			ac:           &config.AutoContinueConfig{},
			multiSession: true,
			haveEventlog: true,
			wantEnabled:  true,
			wantRetry:    true,
			wantWarnSub:  "on by default",
		},
		// --- explicit false (hard opt-out) ---
		{
			name:         "explicitFalse+allPreconditions=off",
			ac:           &config.AutoContinueConfig{Enabled: boolPtr(false)},
			multiSession: true,
			haveEventlog: true,
			wantEnabled:  false,
		},
		// --- explicit true ---
		{
			name:         "explicitTrue+multiSession+eventlog=on",
			ac:           &config.AutoContinueConfig{Enabled: boolPtr(true)},
			multiSession: true,
			haveEventlog: true,
			wantEnabled:  true,
			wantRetry:    true,
		},
		{
			name:         "explicitTrue+REPL+eventlog=off(wrongMode warn)",
			ac:           &config.AutoContinueConfig{Enabled: boolPtr(true)},
			haveEventlog: true,
			wantEnabled:  false,
			wantWarnSub:  "no effect in this mode",
		},
		{
			name:         "explicitTrue+multiSession+noEventlog=off(session-db warn)",
			ac:           &config.AutoContinueConfig{Enabled: boolPtr(true)},
			multiSession: true,
			haveEventlog: false,
			wantEnabled:  false,
			wantWarnSub:  "requires --session-db",
		},
		// explicit true must NOT print the default-on notice
		{
			name:         "explicitTrue does not emit default notice",
			ac:           &config.AutoContinueConfig{Enabled: boolPtr(true)},
			noREPL:       true,
			haveEventlog: true,
			wantEnabled:  true,
			wantRetry:    true,
		},
		// --- retry opt-out survives ---
		{
			name:         "explicitTrue+retryFalse=on but retry off",
			ac:           &config.AutoContinueConfig{Enabled: boolPtr(true), Retry: boolPtr(false)},
			noREPL:       true,
			haveEventlog: true,
			wantEnabled:  true,
			wantRetry:    false,
		},
		// --- parse failures ---
		{
			name:         "badFreshness=error",
			ac:           &config.AutoContinueConfig{Freshness: "nope"},
			multiSession: true,
			haveEventlog: true,
			wantErr:      true,
		},
		{
			name:         "negativeFreshness=error",
			ac:           &config.AutoContinueConfig{Freshness: "-1h"},
			multiSession: true,
			haveEventlog: true,
			wantErr:      true,
		},
		{
			name:         "badRetryInterval=error",
			ac:           &config.AutoContinueConfig{RetryInterval: "nope"},
			multiSession: true,
			haveEventlog: true,
			wantErr:      true,
		},
		{
			name:         "zeroRetryInterval=error",
			ac:           &config.AutoContinueConfig{RetryInterval: "0s"},
			multiSession: true,
			haveEventlog: true,
			wantErr:      true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var warn bytes.Buffer
			res, err := resolveAutoContinue(tc.ac, tc.multiSession, tc.noREPL, tc.haveEventlog, &warn)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (res=%+v)", res)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.enabled != tc.wantEnabled {
				t.Errorf("enabled: want %v, got %v", tc.wantEnabled, res.enabled)
			}
			if tc.wantEnabled && res.retry != tc.wantRetry {
				t.Errorf("retry: want %v, got %v", tc.wantRetry, res.retry)
			}
			got := warn.String()
			if tc.wantWarnSub == "" {
				if got != "" {
					t.Errorf("want no warning, got %q", got)
				}
			} else if !strings.Contains(got, tc.wantWarnSub) {
				t.Errorf("warning: want substring %q, got %q", tc.wantWarnSub, got)
			}
		})
	}
}

// TestResolveAutoContinueDefaults locks the parsed default window/interval
// when the feature is on and no overrides are set.
func TestResolveAutoContinueDefaults(t *testing.T) {
	t.Parallel()
	res, err := resolveAutoContinue(nil, true, false, true, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.enabled {
		t.Fatal("want enabled")
	}
	if res.freshness != time.Hour {
		t.Errorf("freshness default: want 1h, got %v", res.freshness)
	}
	if res.retryInterval != 5*time.Minute {
		t.Errorf("retryInterval default: want 5m, got %v", res.retryInterval)
	}
}
