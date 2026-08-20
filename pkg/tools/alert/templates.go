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
}

// renderTemplate turns the flat tool args into the target service's wire
// format. session is the session the call is running in, or "" when the
// invocation context does not name one.
//
// Two templates ship. config.validateAlerts rejects the rest
// (slack/discord/pagerduty_events_v2) at load time, so an unimplemented
// template cannot reach here in normal operation — the default arm is a
// defense-in-depth backstop for a target that somehow bypassed
// validation.
//
// The generic payload is {"level","summary","details"}. It deliberately
// omits a timestamp: the eventlog entry ADK records for the tool call is
// the authoritative timestamp, and a receiver stamps its own receipt
// time — baking one into the body would only make the output
// nondeterministic. A timestamp field can be added with the ε.2
// service templates if a bespoke receiver needs it.
func renderTemplate(tgt config.AlertTarget, in Args, session string) (rendered, error) {
	switch tgt.Template {
	case config.AlertTemplateGeneric:
		payload := map[string]any{
			"level":   in.Level,
			"summary": in.Summary,
		}
		if len(in.Details) > 0 {
			payload["details"] = in.Details
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return rendered{}, err
		}
		return rendered{body: b, contentType: "application/json"}, nil

	case config.AlertTemplateSwitchboard:
		b, err := json.Marshal(map[string]any{
			"conversation": tgt.Conversation,
			"text":         switchboardText(in),
		})
		if err != nil {
			return rendered{}, err
		}
		out := rendered{body: b, contentType: "application/json"}
		if session != "" {
			out.header = map[string]string{sessionHeader: session}
		}
		return out, nil

	default:
		return rendered{}, fmt.Errorf("template %q is not implemented in this build", tgt.Template)
	}
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
	for _, k := range slices.Sorted(maps.Keys(in.Details)) {
		fmt.Fprintf(&b, "\n- `%s`: %s", k, detailValue(in.Details[k]))
	}
	return b.String()
}

// detailValue renders one detail for the markdown body. Strings pass
// through as themselves — quoting every value would put "" around half
// an incident report. Everything else becomes compact JSON, which beats
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
