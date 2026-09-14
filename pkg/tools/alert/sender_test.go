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

package alert

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

func TestSenderDelivers(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "ok", &got)
	cfg := cfgWith(config.AlertTarget{Name: "oncall", URL: srv.URL, Template: config.AlertTemplateGeneric})

	s, err := newSender(cfg, "oncall", func(string) string { return "" }, nil, srv.Client())
	if err != nil {
		t.Fatalf("newSender: %v", err)
	}
	if s.Target() != "oncall" {
		t.Fatalf("Target() = %q, want oncall", s.Target())
	}
	err = s.Send(context.Background(), "warning", "a prompt is waiting",
		map[string]any{"request_id": "abc123"}, "sess-1")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("unmarshal body: %v (%s)", err, got.body)
	}
	if body["summary"] != "a prompt is waiting" {
		t.Errorf("summary = %v", body["summary"])
	}
	if !strings.Contains(string(got.body), "abc123") {
		t.Errorf("details did not reach the wire: %s", got.body)
	}
}

// The two ways a target can be unusable are two different operator
// mistakes, and collapsing them sends somebody who spelled the name
// right off hunting for a typo.
func TestSenderDistinguishesUnknownFromUndeliverable(t *testing.T) {
	t.Parallel()
	cfg := cfgWith(
		config.AlertTarget{Name: "oncall", URLEnv: "TEST_HOOK_URL", Template: config.AlertTemplateGeneric},
	)
	getenv := func(string) string { return "" } // TEST_HOOK_URL unset

	if _, err := newSender(cfg, "onkall", getenv, nil, nil); err == nil {
		t.Fatal("want an error for an unknown target name")
	} else if !strings.Contains(err.Error(), "unknown target") || !strings.Contains(err.Error(), "oncall") {
		t.Errorf("unknown-target error should name the target and list the real ones, got: %v", err)
	}

	if _, err := newSender(cfg, "oncall", getenv, nil, nil); err == nil {
		t.Fatal("want an error for a target whose env is unset")
	} else if !strings.Contains(err.Error(), "cannot be delivered") {
		t.Errorf("undeliverable error should say so rather than claim the name is unknown, got: %v", err)
	}
}

// The runtime's notification budget must not be spendable by the model.
// If the two shared a limiter, an agent firing alerts in a loop could
// exhaust the budget for the channel that announces the gate holding
// that same agent — a denial of notification the agent controls.
func TestSenderBudgetIsNotTheToolsBudget(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "ok", &got)
	cfg := cfgWith(config.AlertTarget{Name: "oncall", URL: srv.URL, Template: config.AlertTemplateGeneric})
	cfg.Alerts.RateLimitPerTarget = "1/hour" // one send per window, per limiter
	getenv := func(string) string { return "" }

	h, err := newHandler(yoloGate(t), cfg, getenv, nil, srv.Client())
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	s, err := newSender(cfg, "oncall", getenv, nil, srv.Client())
	if err != nil {
		t.Fatalf("newSender: %v", err)
	}

	// The model spends the tool's whole budget.
	in := Args{Target: "oncall", Level: "info", Summary: "chatter"}
	if _, err := h.run(tool.Context(nil), in); err != nil {
		t.Fatalf("first tool call: %v", err)
	}
	if _, err := h.run(tool.Context(nil), in); err == nil {
		t.Fatal("the tool's own budget should now be spent")
	}

	// The runtime's is untouched.
	if err := s.Send(context.Background(), "warning", "a prompt is waiting", nil, "sess-1"); err != nil {
		t.Fatalf("the sender must not draw from the tool's budget: %v", err)
	}
}

// A rate-limited send is not a delivered one, and reporting nil would
// tell the caller somebody was told.
func TestSenderRateLimitIsAnError(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "ok", &got)
	cfg := cfgWith(config.AlertTarget{Name: "oncall", URL: srv.URL, Template: config.AlertTemplateGeneric})
	cfg.Alerts.RateLimitPerTarget = "1/hour"
	s, err := newSender(cfg, "oncall", func(string) string { return "" }, nil, srv.Client())
	if err != nil {
		t.Fatalf("newSender: %v", err)
	}
	if err := s.Send(context.Background(), "warning", "one", nil, ""); err != nil {
		t.Fatalf("first send: %v", err)
	}
	err = s.Send(context.Background(), "warning", "two", nil, "")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second send err = %v, want ErrRateLimited", err)
	}
}

func TestSenderRejectsABadLevel(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "ok", &got)
	cfg := cfgWith(config.AlertTarget{Name: "oncall", URL: srv.URL, Template: config.AlertTemplateGeneric})
	s, err := newSender(cfg, "oncall", func(string) string { return "" }, nil, srv.Client())
	if err != nil {
		t.Fatalf("newSender: %v", err)
	}
	if err := s.Send(context.Background(), "URGENT", "x", nil, ""); err == nil {
		t.Fatal("want an error for an out-of-set level")
	}
}
