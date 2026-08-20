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
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// cfgWithSyncWait builds the smallest config that carries a
// tools.spawn_agent.sync_wait_timeout value.
func cfgWithSyncWait(raw string) *config.Config {
	c := &config.Config{}
	c.Tools.SpawnAgent.SyncWaitTimeout = raw
	return c
}

// TestResolveSyncWaitTimeout is the #692 guard. Before it,
// defaultSyncWaitTimeout was a const wired straight into
// background.WithSyncWaitTimeout, so an operator whose subagents run
// longer than five minutes had no way to say so — the cap fired
// mid-investigation and the parent redid the work itself.
func TestResolveSyncWaitTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		flag string
		cfg  *config.Config
		want time.Duration
	}{
		{
			// Nothing set anywhere: the pre-#692 behavior, unchanged.
			name: "unset falls back to the built-in default",
			cfg:  &config.Config{},
			want: defaultSyncWaitTimeout,
		},
		{
			name: "config raises it",
			cfg:  cfgWithSyncWait("15m"),
			want: 15 * time.Minute,
		},
		{
			name: "flag raises it",
			flag: "20m",
			cfg:  &config.Config{},
			want: 20 * time.Minute,
		},
		{
			// Flag beats config, as everywhere else in this binary.
			name: "flag beats config",
			flag: "20m",
			cfg:  cfgWithSyncWait("15m"),
			want: 20 * time.Minute,
		},
		{
			// 0 is not "unset": background.WithSyncWaitTimeout(0)
			// means wait until the subagent's own budgets end it, and
			// promoting it back to 5m would leave that unsayable.
			name: "explicit zero means no cap, not the default",
			cfg:  cfgWithSyncWait("0s"),
			want: 0,
		},
		{
			name: "whitespace-only is treated as unset",
			cfg:  cfgWithSyncWait("   "),
			want: defaultSyncWaitTimeout,
		},
		{
			// A config value must not leak through when the flag is
			// the empty string but the operator did pass one earlier
			// in the same struct — the flag is only consulted when
			// non-empty.
			name: "empty flag defers to config",
			flag: "",
			cfg:  cfgWithSyncWait("90s"),
			want: 90 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveSyncWaitTimeout(tc.flag, tc.cfg)
			if err != nil {
				t.Fatalf("resolveSyncWaitTimeout(%q): %v", tc.flag, err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// A bad value fails startup naming the surface it came from. Silently
// falling back to 5m would hand the operator the exact behavior they
// were trying to change, with nothing said about it.
func TestResolveSyncWaitTimeout_RejectsBadValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		flag      string
		cfg       *config.Config
		wantNames string
	}{
		{
			name:      "unparseable flag names the flag",
			flag:      "ten minutes",
			cfg:       &config.Config{},
			wantNames: "--subagent-sync-wait",
		},
		{
			name:      "unparseable config names the config field",
			cfg:       cfgWithSyncWait("15"),
			wantNames: "tools.spawn_agent.sync_wait_timeout",
		},
		{
			// Negative would sail into an already-expired context and
			// make every synchronous spawn look instantly timed out.
			name:      "negative is refused",
			cfg:       cfgWithSyncWait("-1m"),
			wantNames: "tools.spawn_agent.sync_wait_timeout",
		},
		{
			name:      "negative on the flag is refused too",
			flag:      "-30s",
			cfg:       &config.Config{},
			wantNames: "--subagent-sync-wait",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveSyncWaitTimeout(tc.flag, tc.cfg)
			if err == nil {
				t.Fatal("want an error, got none")
			}
			if !strings.Contains(err.Error(), tc.wantNames) {
				t.Errorf("error %q does not name %q — an operator can't fix what isn't named", err, tc.wantNames)
			}
		})
	}
}

// A nil config is what the resolver sees before a config file is
// loaded; it must not panic there.
func TestResolveSyncWaitTimeout_NilConfig(t *testing.T) {
	t.Parallel()
	got, err := resolveSyncWaitTimeout("", nil)
	if err != nil {
		t.Fatalf("resolveSyncWaitTimeout: %v", err)
	}
	if got != defaultSyncWaitTimeout {
		t.Errorf("got %v, want %v", got, defaultSyncWaitTimeout)
	}
}
