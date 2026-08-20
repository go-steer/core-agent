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
	"unicode/utf8"
)

// Discord's documented embed limits. Exceeding any of them is a 400 on
// the whole webhook post.
const (
	discordTitleMax      = 256  // embed.title
	discordDescMax       = 4096 // embed.description
	discordFieldNameMax  = 256  // embed.fields[].name
	discordFieldValueMax = 1024 // embed.fields[].value
	discordMaxFields     = 25   // embed.fields
	discordEmbedTotalMax = 6000 // title + description + every field name and value
)

// discordOverflowBudget reserves room for the "N more omitted" field so
// naming the truncation cannot itself blow the total-character limit.
const discordOverflowBudget = 160

// discordColors maps the tool's severity levels to the embed's left
// stripe. Colour is the one thing an embed gives that a plain webhook
// post does not, and severity is what a channel scanning past an alert
// needs to read without stopping.
var discordColors = map[string]int{
	"info":     0x3498DB, // blue
	"warning":  0xF1C40F, // amber
	"critical": 0xE74C3C, // red
	"resolved": 0x2ECC71, // green
}

// discordPayload renders the alert as a single Discord webhook embed.
//
// The title carries the level and summary; when the summary is too long
// for a title the full text moves into the description rather than being
// dropped, so a long summary costs a line of layout and never costs
// information. Details become inline fields, sorted.
//
// Values are NOT escaped, unlike the Slack template. Discord's markup is
// cosmetic — a stray asterisk italicises a word — whereas Slack's "<"
// opens a link that swallows the rest of the value. Escaping every
// asterisk and underscore in a stack trace or a label selector costs
// more legibility than it buys.
func discordPayload(in Args) map[string]any {
	full := fmt.Sprintf("[%s] %s", in.Level, in.Summary)
	embed := map[string]any{
		"title": truncate(full, discordTitleMax),
		"color": discordColors[in.Level],
	}
	// Discord counts characters, not bytes, so the running total does too.
	used := utf8.RuneCountInString(truncate(full, discordTitleMax))
	if utf8.RuneCountInString(full) > discordTitleMax {
		desc := truncate(full, discordDescMax)
		embed["description"] = desc
		used += utf8.RuneCountInString(desc)
	}

	keys := sortedKeys(in.Details)
	dropped := 0
	if len(keys) > discordMaxFields {
		dropped = len(keys) - discordMaxFields
		keys = keys[:discordMaxFields]
	}

	fields := make([]any, 0, len(keys))
	for i, k := range keys {
		name := truncate(nonEmpty(k), discordFieldNameMax)
		value := truncate(nonEmpty(detailValue(in.Details[k])), discordFieldValueMax)
		// Stop before the 6000-character total rather than after: the
		// remaining keys are counted into `dropped` and reported.
		cost := utf8.RuneCountInString(name) + utf8.RuneCountInString(value)
		if used+cost > discordEmbedTotalMax-discordOverflowBudget {
			dropped += len(keys) - i
			break
		}
		used += cost
		fields = append(fields, map[string]any{"name": name, "value": value, "inline": true})
	}
	if dropped > 0 {
		fields = append(fields, map[string]any{
			"name":  "…",
			"value": fmt.Sprintf("%d more detail(s) omitted — Discord caps an embed at %d fields and %d characters.", dropped, discordMaxFields, discordEmbedTotalMax),
		})
	}
	if len(fields) > 0 {
		embed["fields"] = fields
	}

	return map[string]any{"embeds": []any{embed}}
}

// nonEmpty substitutes a visible placeholder for the empty string.
// Discord rejects an embed field with an empty name or value, and a
// detail whose value renders empty ("" from the model, or a key that is
// itself "") must not take the whole alert down with it.
func nonEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
