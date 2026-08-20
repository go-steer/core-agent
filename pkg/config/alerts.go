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
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Alert template kinds, all of them implemented in pkg/tools/alert.
//
// "generic" is a raw JSON pass-through for a bespoke receiver. "slack",
// "discord" and "pagerduty_events_v2" are service formats: one platform
// each, posted straight at that platform's webhook.
//
// "switchboard" is the odd one out — a destination CLASS, not a service
// format. The go-steer/switchboard chat gateway bridges Slack and Google
// Chat today and more later, translating one markdown body per platform,
// so one template covers all of them and the message lands in a thread a
// human can reply in. The "slack" template is the different thing: Block
// Kit fired into a channel, fire-and-forget, with nothing coming back.
const (
	AlertTemplateGeneric           = "generic"
	AlertTemplateSwitchboard       = "switchboard"
	AlertTemplateSlack             = "slack"
	AlertTemplateDiscord           = "discord"
	AlertTemplatePagerDutyEventsV2 = "pagerduty_events_v2"
)

// AlertTemplates is every template validateAlerts accepts, in the order
// error messages list them. pkg/tools/alert ranges over it to prove its
// renderer knows each one: a template the config layer admits and the
// renderer does not is a daemon that boots and then fails at the moment
// the alert is fired.
var AlertTemplates = []string{
	AlertTemplateGeneric,
	AlertTemplateSwitchboard,
	AlertTemplateSlack,
	AlertTemplateDiscord,
	AlertTemplatePagerDutyEventsV2,
}

// alertTemplateList renders AlertTemplates for an error message.
var alertTemplateList = `"` + strings.Join(AlertTemplates, `", "`) + `"`

// AlertsConfig declares the named webhook targets the native `alert`
// tool (pkg/tools/alert) can fire, plus an optional per-target rate
// limit. When Targets is empty the tool is not registered at all — the
// model never sees an `alert` in its schema (the fetch_url pattern).
//
// The design rationale (SSRF-safe by construction, distroless-native
// escalation, audit through the eventlog) lives in
// docs/alert-tool-design.md.
type AlertsConfig struct {
	// Targets is the operator-declared allow-list of escalation
	// destinations. The agent fires a target by name; there is no
	// arbitrary-URL parameter, so a hallucinated target name is
	// rejected rather than dialed (SSRF-safe by construction).
	Targets []AlertTarget `json:"targets,omitempty"`
	// RateLimitPerTarget bounds how often a SINGLE target can fire,
	// to catch pathological alert loops (not to enforce operational
	// cadence — distinct targets are independent). Empty = no limit.
	// Format: a Go duration ("30s" = 1 per 30s) or "N/window"
	// ("5/min", "100/hour"). See ParseAlertRateLimit.
	RateLimitPerTarget string `json:"rate_limit_per_target,omitempty"`
}

// AlertTarget is one named escalation destination.
type AlertTarget struct {
	// Name is the identifier the agent uses and the suffix in
	// permissions.allow patterns ("alert:<name>"). [A-Za-z0-9_-]{1,64}.
	Name string `json:"name"`
	// Kind is the transport. "" or "webhook" today; the field exists
	// so future kinds (smtp, polling APIs) can be added without a
	// schema break.
	Kind string `json:"kind,omitempty"`
	// URL or URLEnv — exactly one. Prefer URLEnv for any destination
	// whose URL embeds a secret (Slack Incoming Webhooks put the token
	// in the path), so the secret stays in a K8s Secret / env var.
	URL    string `json:"url,omitempty"`
	URLEnv string `json:"url_env,omitempty"`
	// Template selects the wire format (see AlertTemplate* constants).
	Template string `json:"template"`
	// Conversation is the destination conversation, for templates that
	// address one: required by "switchboard", rejected by every other
	// template so a field that would silently do nothing cannot be set.
	//
	// It is the gateway's platform-specific conversation key — for Slack
	// a channel ID ("C0123") or "channel:thread_ts" to post into an
	// existing thread. Routing config, like the URL, so it lives beside
	// the URL rather than in the model's arguments: the same reason the
	// tool has no arbitrary-URL parameter, one level in. A literal (no
	// *_env sibling) because a conversation key is not a secret — unlike
	// a Slack Incoming Webhook URL, which carries its token in the path.
	Conversation string `json:"conversation,omitempty"`
	// Auth is optional per-target auth material, resolved from env at
	// call time. Absent = no auth headers (the Slack Incoming Webhook
	// case — the token is already in the URL).
	Auth *AlertAuth `json:"auth,omitempty"`
	// Description is a human/LLM-facing hint surfaced in the tool
	// schema so the model knows what each target is for.
	Description string `json:"description,omitempty"`
}

// AlertAuth carries per-target auth, as NAMES of env vars (never the
// secret itself) so credentials stay out of config files and audit rows.
// Set either BearerEnv, or both BasicEnvUser+BasicEnvPass — not both
// schemes.
type AlertAuth struct {
	BearerEnv    string `json:"bearer_env,omitempty"`
	BasicEnvUser string `json:"basic_env_user,omitempty"`
	BasicEnvPass string `json:"basic_env_pass,omitempty"`
}

// validateAlerts checks the alerts block. Structural only (per Validate's
// contract): whether an env var is actually set, or a URL reachable, is a
// call-time concern for the tool, not a load-time one.
func (c *Config) validateAlerts() error {
	seen := make(map[string]struct{}, len(c.Alerts.Targets))
	for i, t := range c.Alerts.Targets {
		if t.Name == "" {
			return fmt.Errorf("config: alerts.targets[%d].name is required", i)
		}
		if !validAlertName(t.Name) {
			return fmt.Errorf("config: alerts.targets[%d].name=%q must be [A-Za-z0-9_-]{1,64} (the agent fires it by name; it also appears in permissions.allow patterns like \"alert:%s\")", i, t.Name, t.Name)
		}
		if _, dup := seen[t.Name]; dup {
			return fmt.Errorf("config: alerts.targets[%d]: duplicate name %q", i, t.Name)
		}
		seen[t.Name] = struct{}{}

		switch t.Kind {
		case "", "webhook":
			// ok; "" defaults to webhook.
		default:
			return fmt.Errorf("config: alerts.targets[%d] (%q): kind=%q is not supported (only \"webhook\" ships today)", i, t.Name, t.Kind)
		}

		// Exactly one of url / url_env.
		if (t.URL == "") == (t.URLEnv == "") {
			return fmt.Errorf("config: alerts.targets[%d] (%q): set exactly one of url or url_env", i, t.Name)
		}
		if t.URL != "" {
			u, err := url.Parse(t.URL)
			if err != nil {
				return fmt.Errorf("config: alerts.targets[%d] (%q): url %q: %v", i, t.Name, t.URL, err)
			}
			switch strings.ToLower(u.Scheme) {
			case "http", "https":
			default:
				return fmt.Errorf("config: alerts.targets[%d] (%q): url must be http(s), got scheme %q", i, t.Name, u.Scheme)
			}
			if u.Host == "" {
				return fmt.Errorf("config: alerts.targets[%d] (%q): url %q has no host", i, t.Name, t.URL)
			}
		}

		switch t.Template {
		case AlertTemplateGeneric:
			// ok.
		case AlertTemplateSwitchboard:
			// The gateway needs somewhere to put the message and a token
			// to be let in at all, and neither can be supplied later:
			// the model has no say in either, and switchboard's ingress
			// refuses to start without a token, so a target missing one
			// is an escalation path that can only 401 at 3am. Both are
			// structural, so both fail the load rather than the fire.
			if t.Conversation == "" {
				return fmt.Errorf("config: alerts.targets[%d] (%q): template=%q requires conversation (the gateway's conversation key — for Slack a channel ID like \"C0123\", or \"channel:thread_ts\" to post into a thread)", i, t.Name, t.Template)
			}
			if hasBlankOrControl(t.Conversation) {
				return fmt.Errorf("config: alerts.targets[%d] (%q): conversation=%q must not contain whitespace or control characters (it is an opaque platform key, never prose)", i, t.Name, t.Conversation)
			}
			if t.Auth == nil || t.Auth.BearerEnv == "" {
				return fmt.Errorf("config: alerts.targets[%d] (%q): template=%q requires auth.bearer_env (switchboard's ingress token, deliberately distinct from the daemon token; its ingress refuses every unauthenticated post)", i, t.Name, t.Template)
			}
		case AlertTemplateSlack, AlertTemplateDiscord:
			// Nothing extra: the Incoming Webhook URL is the whole
			// credential and the whole destination, which is why these
			// two normally carry no auth block at all.
		case AlertTemplatePagerDutyEventsV2:
			// The routing key is the integration's identity AND its
			// credential, and Events v2 reads it from the body — so it is
			// required here for the same reason switchboard's token is:
			// without it the target can only ever be rejected, and being
			// a *_env it is also what drops the target at startup when
			// the Secret is missing.
			if t.Auth == nil || t.Auth.BearerEnv == "" {
				return fmt.Errorf("config: alerts.targets[%d] (%q): template=%q requires auth.bearer_env (the PagerDuty integration's routing key; it is sent in the request body, not as a bearer token)", i, t.Name, t.Template)
			}
		case "":
			return fmt.Errorf("config: alerts.targets[%d] (%q): template is required (want one of %s)", i, t.Name, alertTemplateList)
		default:
			return fmt.Errorf("config: alerts.targets[%d] (%q): template=%q is unknown (want one of %s)", i, t.Name, t.Template, alertTemplateList)
		}

		// Refused rather than ignored: a conversation on a template that
		// cannot address one is a config that reads like it routes
		// somewhere and does not.
		if t.Conversation != "" && t.Template != AlertTemplateSwitchboard {
			return fmt.Errorf("config: alerts.targets[%d] (%q): conversation is only meaningful for template=%q, not %q", i, t.Name, AlertTemplateSwitchboard, t.Template)
		}

		if a := t.Auth; a != nil {
			hasBearer := a.BearerEnv != ""
			hasBasic := a.BasicEnvUser != "" || a.BasicEnvPass != ""
			switch {
			case hasBearer && hasBasic:
				return fmt.Errorf("config: alerts.targets[%d] (%q): set either auth.bearer_env OR auth.basic_env_user+basic_env_pass, not both", i, t.Name)
			case hasBasic && (a.BasicEnvUser == "" || a.BasicEnvPass == ""):
				return fmt.Errorf("config: alerts.targets[%d] (%q): auth basic requires BOTH basic_env_user and basic_env_pass", i, t.Name)
			case !hasBearer && !hasBasic:
				return fmt.Errorf("config: alerts.targets[%d] (%q): auth is set but empty — omit the auth block entirely for no-auth targets", i, t.Name)
			}
		}
	}

	if c.Alerts.RateLimitPerTarget != "" {
		if _, _, err := ParseAlertRateLimit(c.Alerts.RateLimitPerTarget); err != nil {
			return fmt.Errorf("config: alerts.rate_limit_per_target=%q: %w", c.Alerts.RateLimitPerTarget, err)
		}
	}
	return nil
}

// validAlertName reports whether s is safe as a target name (the agent
// fires it by name; it appears in gate keys and error messages).
// Mirrors validSubagentName's hand-rolled charset to keep this
// foundational package regexp-free.
func validAlertName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// hasBlankOrControl reports whether s holds whitespace or a control
// character. Mirrors the check switchboard's ingress runs on a
// conversation key, so a typo'd key fails the boot here instead of
// returning 400 from the gateway during the incident it was meant to
// escalate.
func hasBlankOrControl(s string) bool {
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// ParseAlertRateLimit parses an alerts.rate_limit_per_target spec into a
// (count, window) pair meaning "count alerts per window". Accepted forms:
//
//	"30s"      → 1 per 30s   (a bare Go duration is shorthand for "1/<dur>")
//	"1/30s"    → 1 per 30s
//	"5/min"    → 5 per minute
//	"100/hour" → 100 per hour
//
// The window accepts Go duration syntax ("30s", "1m30s", "2h") plus the
// friendly bare units "sec"/"min"/"hour"/"day" (and "s"/"m"/"h"/"d")
// that time.ParseDuration rejects on their own. An empty string is an
// error; callers treat "" as "no limit" before calling.
func ParseAlertRateLimit(s string) (count int, window time.Duration, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, fmt.Errorf("empty rate limit")
	}
	count = 1
	windowSpec := s
	if i := strings.IndexByte(s, '/'); i >= 0 {
		countStr := strings.TrimSpace(s[:i])
		windowSpec = strings.TrimSpace(s[i+1:])
		n, cerr := strconv.Atoi(countStr)
		if cerr != nil {
			return 0, 0, fmt.Errorf("count %q is not an integer", countStr)
		}
		if n <= 0 {
			return 0, 0, fmt.Errorf("count must be > 0, got %d", n)
		}
		count = n
	}
	window, err = parseAlertWindow(windowSpec)
	if err != nil {
		return 0, 0, err
	}
	return count, window, nil
}

func parseAlertWindow(spec string) (time.Duration, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, fmt.Errorf("missing duration window (e.g. 30s, 1/30s, 5/min)")
	}
	// Friendly bare units that time.ParseDuration rejects on their own.
	switch strings.ToLower(spec) {
	case "s", "sec", "second":
		return time.Second, nil
	case "m", "min", "minute":
		return time.Minute, nil
	case "h", "hr", "hour":
		return time.Hour, nil
	case "d", "day":
		return 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(spec)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use e.g. 30s, 1/30s, 5/min, 100/hour)", spec)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be > 0, got %s", d)
	}
	return d, nil
}
