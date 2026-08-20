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
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// sessionHeader names the session an alert was fired from.
//
// It rides a header rather than the JSON body because switchboard's
// ingress decodes with DisallowUnknownFields — a deliberate choice on
// its side ("this contract is young enough to be worth policing") that
// makes an unrecognised body field a 400, not a no-op. A header is the
// part of an HTTP request that is defined to be ignorable when unknown,
// so this delivers to today's gateway and is readable by tomorrow's
// without either end negotiating a version. Switchboard already takes
// caller metadata this way (Idempotency-Key).
//
// Only the switchboard template sends it: a session id is internal
// routing detail, and there is no reason for a third-party webhook to
// learn one.
const sessionHeader = "X-Agent-Session"

// rendered is one alert's HTTP request, minus the destination: the body,
// its Content-Type, and any headers the template needs alongside it.
// Auth is NOT here — it is resolved from env in applyAuth, after these
// headers are set, so no template can shadow the Authorization header.
type rendered struct {
	body        []byte
	contentType string
	header      map[string]string
	// omitAuthHeader suppresses the per-target Authorization header for
	// templates whose credential is not one. PagerDuty Events v2 reads
	// its routing key from the BODY and never inspects Authorization, so
	// the same secret would otherwise go out twice, the second time
	// mislabelled as a bearer token. The template resolves the key
	// itself, so the fail-closed check applyAuth would have run has
	// already happened by the time this is set.
	omitAuthHeader bool
}

// renderEnv is what a template needs about the invocation beyond the
// target and the args: the session it ran in ("" when the context names
// none) and the env lookup, for templates whose credential belongs in
// the payload rather than a header.
type renderEnv struct {
	session string
	getenv  func(string) string
}

// renderTemplate turns the flat tool args into the target service's wire
// format.
//
// Every template config.validateAlerts accepts is implemented here, and
// the two lists are pinned together by TestEveryAcceptedTemplateRenders:
// a template the validator lets through but this switch does not know is
// a config that boots and then fails at the moment it is used. The
// default arm is the backstop for a target that somehow bypassed
// validation entirely.
//
// The generic payload is {"level","summary","details"}. It deliberately
// omits a timestamp: the eventlog entry ADK records for the tool call is
// the authoritative timestamp, and a receiver stamps its own receipt
// time — baking one into the body would only make the output
// nondeterministic. #749 revisited the question the design doc left open
// and kept the omission, because none of the three service templates
// wants one either (Slack and Discord stamp the post; PagerDuty stamps
// the event and accepts an explicit payload.timestamp only for
// backfill).
func renderTemplate(tgt config.AlertTarget, in Args, env renderEnv) (rendered, error) {
	switch tgt.Template {
	case config.AlertTemplateGeneric:
		payload := map[string]any{
			"level":   in.Level,
			"summary": in.Summary,
		}
		if len(in.Details) > 0 {
			payload["details"] = in.Details
		}
		return jsonBody(payload)

	case config.AlertTemplateSwitchboard:
		out, err := jsonBody(map[string]any{
			"conversation": tgt.Conversation,
			"text":         switchboardText(in),
		})
		if err != nil {
			return rendered{}, err
		}
		if env.session != "" {
			out.header = map[string]string{sessionHeader: env.session}
		}
		return out, nil

	case config.AlertTemplateSlack:
		return jsonBody(slackPayload(in))

	case config.AlertTemplateDiscord:
		return jsonBody(discordPayload(in))

	case config.AlertTemplatePagerDutyEventsV2:
		payload, err := pagerDutyPayload(tgt, in, env.getenv)
		if err != nil {
			return rendered{}, err
		}
		out, err := jsonBody(payload)
		if err != nil {
			return rendered{}, err
		}
		out.omitAuthHeader = true
		return out, nil

	default:
		return rendered{}, fmt.Errorf("template %q is not implemented in this build", tgt.Template)
	}
}

// jsonBody marshals a payload into a rendered JSON request.
func jsonBody(payload any) (rendered, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return rendered{}, err
	}
	return rendered{body: b, contentType: "application/json"}, nil
}

// switchboardText renders the alert as the markdown switchboard expects
// in `text`. The gateway owns the per-platform translation (Slack
// mrkdwn, Google Chat), so this stays CommonMark and stays plain: bold
// for the level, a bullet per detail with the key in backticks. Anything
// richer would be a rendering decision made in the wrong process.
//
// Details are emitted in sorted key order — a map's range order is
// random, and an alert body that differs run to run is one no receiver
// can diff and no test can pin.
func switchboardText(in Args) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**[%s]** %s", in.Level, in.Summary)
	for _, k := range sortedKeys(in.Details) {
		fmt.Fprintf(&b, "\n- `%s`: %s", k, detailValue(in.Details[k]))
	}
	return b.String()
}

// sortedKeys returns the detail keys in sorted order. Every template
// iterates details through this: a Go map's range order is random, and
// an alert body that differs run to run is one no receiver can diff and
// no test can pin.
func sortedKeys(details map[string]any) []string {
	return slices.Sorted(maps.Keys(details))
}

// detailValue renders one detail for a text body. Strings pass through
// as themselves — quoting every value would put "" around half an
// incident report. Everything else becomes compact JSON, which beats
// Go's %v for a nested map: a reader can paste it into a parser, and
// nothing unexpected prints as an address. %v is the last resort for the
// values json cannot encode (a channel, a func) that no decoded tool
// argument can actually be.
func detailValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

// truncate caps s at max RUNES, marking the cut with an ellipsis.
//
// Rune-counted rather than byte-counted for two reasons: Slack, Discord
// and PagerDuty all document their field caps in characters, and a
// byte-wise cut can split a multi-byte rune and put an invalid UTF-8
// sequence on the wire. Every service limit these templates respect goes
// through here, because a body that exceeds one is a 400 at the moment
// the alert mattered — the model has no idea a field it filled is too
// long, and it should not have to.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}
