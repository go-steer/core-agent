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
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// PagerDuty Events v2 limits and defaults.
const (
	pdSummaryMax  = 1024 // payload.summary
	pdDedupKeyMax = 255  // dedup_key
	// pdDefaultSource is what payload.source falls back to. PagerDuty
	// requires the field and shows it as "the affected system"; a
	// deployment that can say something better sets details.source.
	pdDefaultSource = "core-agent"
)

// pdDetailSource and pdDetailDedupKey are the two detail keys this
// template PROMOTES out of custom_details into first-class Events v2
// fields. They are ordinary details so the model needs no
// template-specific argument, and they are removed from custom_details
// once promoted so the same value does not appear twice in the incident.
const (
	pdDetailSource   = "source"
	pdDetailDedupKey = "dedup_key"
)

// pdSeverity maps the tool's levels onto the four severities Events v2
// accepts. "resolved" has no severity of its own — it is an event_action,
// and PagerDuty ignores the payload on a resolve — so it maps to the
// least noisy value rather than inventing one.
var pdSeverity = map[string]string{
	"info":     "info",
	"warning":  "warning",
	"critical": "critical",
	"resolved": "info",
}

// pagerDutyPayload renders the alert as a PagerDuty Events API v2
// enqueue request.
//
// The routing key comes from auth.bearer_env and goes in the BODY, which
// is where Events v2 reads it; the caller suppresses the Authorization
// header for this template, because /v2/enqueue never inspects one and
// sending the same secret twice — the second time mislabelled as a
// bearer token — is a leak to whatever sits in front of the URL. Reusing
// auth.bearer_env rather than adding a routing_key_env field is what
// makes an unset key drop the target at startup under the existing
// undeliverable-targets rule (#762): PagerDuty is the last target that
// should be advertised to the model and then silently unable to page.
func pagerDutyPayload(tgt config.AlertTarget, in Args, getenv func(string) string) (map[string]any, error) {
	if tgt.Auth == nil || tgt.Auth.BearerEnv == "" {
		// Unreachable through a validated config; kept so a caller that
		// builds a target by hand gets a diagnosis, not a page that
		// PagerDuty rejects as unauthorised.
		return nil, errors.New("pagerduty_events_v2 requires auth.bearer_env (the integration's routing key)")
	}
	if getenv == nil {
		return nil, errors.New("pagerduty_events_v2: no environment to resolve the routing key from")
	}
	routingKey := strings.TrimSpace(getenv(tgt.Auth.BearerEnv))
	if routingKey == "" {
		return nil, fmt.Errorf("auth.bearer_env %q is unset or empty (it carries the PagerDuty routing key)", tgt.Auth.BearerEnv)
	}

	action := "trigger"
	if in.Level == "resolved" {
		action = "resolve"
	}

	details := maps.Clone(in.Details)
	if details == nil {
		details = map[string]any{}
	}
	source := pdDefaultSource
	if s, ok := details[pdDetailSource].(string); ok && strings.TrimSpace(s) != "" {
		source = s
		delete(details, pdDetailSource)
	}
	dedupKey := ""
	if s, ok := details[pdDetailDedupKey].(string); ok && strings.TrimSpace(s) != "" {
		dedupKey = truncate(s, pdDedupKeyMax)
		delete(details, pdDetailDedupKey)
	}
	// A resolve with no dedup_key is rejected by PagerDuty — there is
	// nothing to resolve. The tool holds no state between calls (the
	// design's cross-daemon-dedup non-goal), so the key has to come from
	// the caller, and the error says so in terms the model can act on
	// instead of surfacing PagerDuty's "Event object is invalid".
	if action == "resolve" && dedupKey == "" {
		return nil, fmt.Errorf("level=resolved needs details.%s set to the same value the triggering alert used — PagerDuty has no other way to know which incident to resolve", pdDetailDedupKey)
	}

	// run() validates the level against the same closed set, so the
	// fallback is for a hand-built Args only — but an empty severity is
	// a 400 from PagerDuty, and "error" is the honest reading of a
	// severity nobody stated.
	severity, ok := pdSeverity[in.Level]
	if !ok {
		severity = "error"
	}
	payload := map[string]any{
		"summary":  truncate(in.Summary, pdSummaryMax),
		"source":   source,
		"severity": severity,
	}
	if len(details) > 0 {
		payload["custom_details"] = details
	}

	body := map[string]any{
		"routing_key":  routingKey,
		"event_action": action,
		"payload":      payload,
	}
	if dedupKey != "" {
		body["dedup_key"] = dedupKey
	}
	return body, nil
}
