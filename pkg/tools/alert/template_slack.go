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
	"fmt"
	"slices"
	"strings"
)

// Slack's documented Block Kit limits. Exceeding any of them is an
// `invalid_blocks` rejection of the whole message, so each one is
// enforced here rather than discovered during an incident.
const (
	slackMaxBlocks        = 50   // blocks per message
	slackHeaderMax        = 150  // plain_text in a header block
	slackTextMax          = 2000 // any single text object in a section
	slackFieldsPerSection = 10   // section.fields
	slackFallbackMax      = 3000 // the notification-preview `text`
)

// slackMaxFields is how many detail fields fit while leaving room for
// the header block and, if the details overflow, a context block saying
// so.
const slackMaxFields = (slackMaxBlocks - 2) * slackFieldsPerSection

// slackPayload renders the alert as Block Kit for a Slack Incoming
// Webhook: a header carrying the level and summary, then the details as
// two-column section fields.
//
// `text` is set as well as `blocks` and is not redundant — Slack uses it
// for the notification preview and the accessibility fallback, and a
// blocks-only message shows up in a phone notification as "This content
// can't be displayed".
//
// This is the direct-to-platform template. It posts into a channel and
// nothing comes back: the Incoming Webhook URL addresses a channel, not
// a thread, and there is no reply to route anywhere. Escalations that
// want a conversation take the switchboard template instead, which
// reaches Slack through the gateway.
func slackPayload(in Args) map[string]any {
	title := fmt.Sprintf("[%s] %s", in.Level, in.Summary)

	blocks := []any{
		map[string]any{
			"type": "header",
			// plain_text, so this one is NOT mrkdwn-escaped: Slack
			// renders it literally and escaping would show entities.
			"text": map[string]any{"type": "plain_text", "text": truncate(title, slackHeaderMax)},
		},
	}

	keys := sortedKeys(in.Details)
	dropped := 0
	if len(keys) > slackMaxFields {
		dropped = len(keys) - slackMaxFields
		keys = keys[:slackMaxFields]
	}
	for chunk := range slices.Chunk(keys, slackFieldsPerSection) {
		fields := make([]any, 0, len(chunk))
		for _, k := range chunk {
			fields = append(fields, map[string]any{
				"type": "mrkdwn",
				"text": truncate(fmt.Sprintf("*%s:*\n%s", escapeSlack(k), escapeSlack(detailValue(in.Details[k]))), slackTextMax),
			})
		}
		blocks = append(blocks, map[string]any{"type": "section", "fields": fields})
	}
	if dropped > 0 {
		// Named, not silently cut: an operator reading a truncated alert
		// needs to know the alert was truncated.
		blocks = append(blocks, map[string]any{
			"type": "context",
			"elements": []any{map[string]any{
				"type": "mrkdwn",
				"text": fmt.Sprintf("_%d more detail(s) omitted — Slack caps a message at %d blocks._", dropped, slackMaxBlocks),
			}},
		})
	}

	return map[string]any{
		"text":   truncate(title, slackFallbackMax),
		"blocks": blocks,
	}
}

// escapeSlack neutralises the three characters Slack treats as markup in
// mrkdwn text. Detail keys and values come from the model, so they are
// data: an unescaped "<" starts a link and swallows everything up to the
// next ">", which turns a value like "<none>" — what kubectl prints for
// an empty field, so an entirely likely one here — into an invisible
// link. Slack documents exactly these three and no backslash escape, so
// entities are the only mechanism.
func escapeSlack(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
