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
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// pdTarget is the shape validateAlerts accepts for PagerDuty: a routing
// key in auth.bearer_env, which the template puts in the body.
func pdTarget(name, url string) config.AlertTarget {
	return config.AlertTarget{
		Name:     name,
		URL:      url,
		Template: config.AlertTemplatePagerDutyEventsV2,
		Auth:     &config.AlertAuth{BearerEnv: "PD_ROUTING_KEY"},
	}
}

func pdEnv(string) string { return "R0UT1NG-KEY" }

// pdBody renders in against a PagerDuty target and decodes the request.
func pdBody(t *testing.T, in Args) map[string]any {
	t.Helper()
	out, err := renderTemplate(pdTarget("page", "https://events.pagerduty.com/v2/enqueue"), in, renderEnv{getenv: pdEnv})
	if err != nil {
		t.Fatalf("renderTemplate(pagerduty): %v", err)
	}
	if !out.omitAuthHeader {
		t.Error("omitAuthHeader = false; the routing key would be sent twice, once as a bearer token")
	}
	var body map[string]any
	if err := json.Unmarshal(out.body, &body); err != nil {
		t.Fatalf("pagerduty body is not JSON: %v (%s)", err, out.body)
	}
	return body
}

func TestPagerDuty_TriggerShape(t *testing.T) {
	t.Parallel()
	body := pdBody(t, Args{
		Level:   "critical",
		Summary: "checkout-svc has no healthy endpoints",
		Details: map[string]any{"cluster": "prod-us-east", "replicas": 0},
	})
	if body["routing_key"] != "R0UT1NG-KEY" {
		t.Errorf("routing_key = %v, want the resolved auth.bearer_env", body["routing_key"])
	}
	if body["event_action"] != "trigger" {
		t.Errorf("event_action = %v, want trigger", body["event_action"])
	}
	if _, has := body["dedup_key"]; has {
		t.Errorf("dedup_key = %v, want it omitted when the caller supplied none", body["dedup_key"])
	}
	payload, _ := body["payload"].(map[string]any)
	if payload["summary"] != "checkout-svc has no healthy endpoints" {
		t.Errorf("payload.summary = %v", payload["summary"])
	}
	if payload["severity"] != "critical" {
		t.Errorf("payload.severity = %v, want critical", payload["severity"])
	}
	if payload["source"] != pdDefaultSource {
		t.Errorf("payload.source = %v, want the %q default (PagerDuty requires the field)", payload["source"], pdDefaultSource)
	}
	custom, _ := payload["custom_details"].(map[string]any)
	if custom["cluster"] != "prod-us-east" || custom["replicas"] != float64(0) {
		t.Errorf("custom_details = %v, want the details passed through", custom)
	}
}

func TestPagerDuty_SeverityPerLevel(t *testing.T) {
	t.Parallel()
	for level, want := range map[string]string{
		"info": "info", "warning": "warning", "critical": "critical", "resolved": "info",
	} {
		in := Args{Level: level, Summary: "s"}
		if level == "resolved" {
			in.Details = map[string]any{"dedup_key": "k"}
		}
		body := pdBody(t, in)
		payload, _ := body["payload"].(map[string]any)
		if payload["severity"] != want {
			t.Errorf("severity for %q = %v, want %q", level, payload["severity"], want)
		}
	}
}

// TestPagerDuty_PromotesSourceAndDedupKey — both are ordinary details so
// the model needs no template-specific argument, and both are removed
// from custom_details once promoted so the same value is not shown twice
// in the incident.
func TestPagerDuty_PromotesSourceAndDedupKey(t *testing.T) {
	t.Parallel()
	original := map[string]any{"source": "prod-us-east/checkout-svc", "dedup_key": "INC-42", "note": "keep me"}
	body := pdBody(t, Args{Level: "critical", Summary: "down", Details: original})
	if body["dedup_key"] != "INC-42" {
		t.Errorf("dedup_key = %v, want it promoted out of details", body["dedup_key"])
	}
	payload, _ := body["payload"].(map[string]any)
	if payload["source"] != "prod-us-east/checkout-svc" {
		t.Errorf("payload.source = %v, want it promoted out of details", payload["source"])
	}
	custom, _ := payload["custom_details"].(map[string]any)
	if _, has := custom["source"]; has {
		t.Errorf("custom_details still carries source: %v", custom)
	}
	if _, has := custom["dedup_key"]; has {
		t.Errorf("custom_details still carries dedup_key: %v", custom)
	}
	if custom["note"] != "keep me" {
		t.Errorf("custom_details = %v, want the un-promoted details kept", custom)
	}
	// The caller's map is the model's tool arguments; promoting must not
	// mutate it, or the audited args would disagree with what was sent.
	if len(original) != 3 {
		t.Errorf("Args.Details was mutated: %v", original)
	}
}

// TestPagerDuty_ResolveNeedsADedupKey is the interesting failure. The
// tool holds no state between calls (the design's cross-daemon-dedup
// non-goal), so a resolve can only correlate through a key the caller
// supplies — and PagerDuty rejects a resolve without one. Failing here,
// with an error that says what to pass, beats surfacing PagerDuty's
// "Event object is invalid".
func TestPagerDuty_ResolveNeedsADedupKey(t *testing.T) {
	t.Parallel()
	_, err := renderTemplate(pdTarget("page", "https://x"), Args{Level: "resolved", Summary: "recovered"}, renderEnv{getenv: pdEnv})
	if err == nil {
		t.Fatal("renderTemplate(resolved, no dedup_key) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "dedup_key") {
		t.Errorf("error = %v, want it to name details.dedup_key", err)
	}

	body := pdBody(t, Args{Level: "resolved", Summary: "recovered", Details: map[string]any{"dedup_key": "INC-42"}})
	if body["event_action"] != "resolve" || body["dedup_key"] != "INC-42" {
		t.Errorf("resolve body = %v, want event_action=resolve with the caller's dedup_key", body)
	}
}

func TestPagerDuty_TruncatesSummaryAndDedupKey(t *testing.T) {
	t.Parallel()
	body := pdBody(t, Args{
		Level:   "warning",
		Summary: strings.Repeat("s", 2000),
		Details: map[string]any{"dedup_key": strings.Repeat("d", 400)},
	})
	payload, _ := body["payload"].(map[string]any)
	if n := utf8.RuneCountInString(payload["summary"].(string)); n != pdSummaryMax {
		t.Errorf("summary length = %d, want %d", n, pdSummaryMax)
	}
	if n := utf8.RuneCountInString(body["dedup_key"].(string)); n != pdDedupKeyMax {
		t.Errorf("dedup_key length = %d, want %d", n, pdDedupKeyMax)
	}
}

// TestPagerDuty_UnresolvableRoutingKeyFailsClosed — the render, not
// applyAuth, is what holds the fail-closed check for this template,
// since the credential never reaches a header.
func TestPagerDuty_UnresolvableRoutingKeyFailsClosed(t *testing.T) {
	t.Parallel()
	_, err := renderTemplate(pdTarget("page", "https://x"), Args{Level: "info", Summary: "s"}, renderEnv{getenv: func(string) string { return "" }})
	if err == nil || !strings.Contains(err.Error(), "PD_ROUTING_KEY") {
		t.Errorf("renderTemplate with an unset routing key = %v, want an error naming the env var", err)
	}
}

// TestRun_PagerDutySendsNoAuthorizationHeader is the whole point of
// omitAuthHeader: /v2/enqueue never reads Authorization, so sending the
// routing key there too would hand the same secret to whatever sits in
// front of the URL, mislabelled as a bearer token.
func TestRun_PagerDutySendsNoAuthorizationHeader(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 202, `{"status":"success"}`, &got)
	cfg := cfgWith(pdTarget("page", srv.URL))
	h, err := newHandler(yoloGate(t), cfg, pdEnv, nil, srv.Client())
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	if _, err := h.run(inSession("s-4412"), Args{Target: "page", Level: "critical", Summary: "down"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.authHeader != "" {
		t.Errorf("authorization = %q, want none — the routing key belongs in the body", got.authHeader)
	}
	if v := got.header.Get(sessionHeader); v != "" {
		t.Errorf("%s = %q on a pagerduty target, want it absent", sessionHeader, v)
	}
	var body map[string]any
	_ = json.Unmarshal(got.body, &body)
	if body["routing_key"] != "R0UT1NG-KEY" {
		t.Errorf("routing_key = %v, want it in the body", body["routing_key"])
	}
}

// TestPagerDutyTargetIsDroppedWhenTheKeyIsUnset ties the template to the
// #762 rule: auth.bearer_env is required by the validator precisely so
// an unset routing key drops the target at startup instead of promising
// the model a page it cannot send.
func TestPagerDutyTargetIsDroppedWhenTheKeyIsUnset(t *testing.T) {
	t.Parallel()
	cfg := cfgWith(pdTarget("page", "https://events.pagerduty.com/v2/enqueue"))
	live, dead := partitionTargets(cfg, func(string) string { return "" })
	if len(live) != 0 || len(dead) != 1 {
		t.Fatalf("partitionTargets = %d live / %d dead, want the target dropped", len(live), len(dead))
	}
	if !strings.Contains(dead[0].Reason, "PD_ROUTING_KEY") {
		t.Errorf("reason = %q, want it to name the env var", dead[0].Reason)
	}
}

// TestEveryAcceptedTemplateRenders pins the config layer's accept-list to
// this package's switch. A template validateAlerts admits and
// renderTemplate does not know is a daemon that boots clean and then
// fails at the moment the alert is fired.
func TestEveryAcceptedTemplateRenders(t *testing.T) {
	t.Parallel()
	for _, tpl := range config.AlertTemplates {
		tgt := config.AlertTarget{Name: "t", URL: "https://x", Template: tpl, Conversation: "C0123", Auth: &config.AlertAuth{BearerEnv: "TOK"}}
		out, err := renderTemplate(tgt, Args{Level: "critical", Summary: "s"}, renderEnv{session: "s-1", getenv: func(string) string { return "tok" }})
		if err != nil {
			t.Errorf("renderTemplate(%q) = %v, want a rendered body", tpl, err)
			continue
		}
		if len(out.body) == 0 || !json.Valid(out.body) {
			t.Errorf("renderTemplate(%q) body = %q, want valid JSON", tpl, out.body)
		}
	}
}
