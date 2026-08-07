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

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// renderTemplate turns the flat tool args into the target service's wire
// format, returning the request body and its Content-Type.
//
// This release implements only the "generic" template (raw JSON
// pass-through). config.validateAlerts rejects the service-specific
// templates (slack/discord/pagerduty_events_v2) at load time, so an
// unimplemented template cannot reach here in normal operation — the
// default arm is a defense-in-depth backstop for a target that somehow
// bypassed validation.
//
// The generic payload is {"level","summary","details"}. It deliberately
// omits a timestamp: the eventlog entry ADK records for the tool call is
// the authoritative timestamp, and a receiver stamps its own receipt
// time — baking one into the body would only make the output
// nondeterministic. A timestamp field can be added with the ε.2
// service templates if a bespoke receiver needs it.
func renderTemplate(name string, in Args) (body []byte, contentType string, err error) {
	switch name {
	case config.AlertTemplateGeneric:
		payload := map[string]any{
			"level":   in.Level,
			"summary": in.Summary,
		}
		if len(in.Details) > 0 {
			payload["details"] = in.Details
		}
		b, mErr := json.Marshal(payload)
		if mErr != nil {
			return nil, "", mErr
		}
		return b, "application/json", nil
	default:
		return nil, "", fmt.Errorf("template %q is not implemented in this build", name)
	}
}
