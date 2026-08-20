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
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// discordEmbed renders in and returns the single embed Discord would see.
func discordEmbed(t *testing.T, in Args) map[string]any {
	t.Helper()
	out, err := renderTemplate(config.AlertTarget{Name: "ops", Template: config.AlertTemplateDiscord}, in, renderEnv{})
	if err != nil {
		t.Fatalf("renderTemplate(discord): %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.body, &payload); err != nil {
		t.Fatalf("discord body is not JSON: %v (%s)", err, out.body)
	}
	embeds, _ := payload["embeds"].([]any)
	if len(embeds) != 1 {
		t.Fatalf("embeds = %v, want exactly one", payload["embeds"])
	}
	embed, _ := embeds[0].(map[string]any)
	return embed
}

func TestDiscord_TitleColorAndFields(t *testing.T) {
	t.Parallel()
	embed := discordEmbed(t, Args{
		Level:   "critical",
		Summary: "checkout-svc has no healthy endpoints",
		Details: map[string]any{"cluster": "prod-us-east", "replicas": 0},
	})
	if embed["title"] != "[critical] checkout-svc has no healthy endpoints" {
		t.Errorf("title = %v, want the level-prefixed summary", embed["title"])
	}
	if got, want := embed["color"], float64(0xE74C3C); got != want {
		t.Errorf("color = %v, want %v (red for critical)", got, want)
	}
	if _, has := embed["description"]; has {
		t.Errorf("description = %v, want it omitted when the title fits", embed["description"])
	}
	fields, _ := embed["fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("fields = %v, want one per detail", fields)
	}
	first, _ := fields[0].(map[string]any)
	if first["name"] != "cluster" || first["value"] != "prod-us-east" || first["inline"] != true {
		t.Errorf("fields[0] = %v, want the sorted-first detail inline", first)
	}
	second, _ := fields[1].(map[string]any)
	if second["name"] != "replicas" || second["value"] != "0" {
		t.Errorf("fields[1] = %v, want replicas rendered as JSON", second)
	}
}

func TestDiscord_ColorPerLevel(t *testing.T) {
	t.Parallel()
	for level, want := range map[string]float64{
		"info": 0x3498DB, "warning": 0xF1C40F, "critical": 0xE74C3C, "resolved": 0x2ECC71,
	} {
		embed := discordEmbed(t, Args{Level: level, Summary: "s"})
		if embed["color"] != want {
			t.Errorf("color for %q = %v, want %v", level, embed["color"], want)
		}
	}
}

// TestDiscord_LongSummaryMovesIntoTheDescription — the title caps at 256
// characters, so a long summary would be silently cut in half. Moving
// the full text into the description costs a line of layout and never
// costs information.
func TestDiscord_LongSummaryMovesIntoTheDescription(t *testing.T) {
	t.Parallel()
	summary := strings.Repeat("z", 900)
	embed := discordEmbed(t, Args{Level: "warning", Summary: summary})
	title, _ := embed["title"].(string)
	if n := utf8.RuneCountInString(title); n != discordTitleMax {
		t.Errorf("title length = %d, want %d", n, discordTitleMax)
	}
	desc, ok := embed["description"].(string)
	if !ok {
		t.Fatalf("description missing; the tail of the summary was dropped: %v", embed)
	}
	if !strings.Contains(desc, summary) {
		t.Errorf("description = %.60q…, want the summary in full", desc)
	}
}

func TestDiscord_FieldLimits(t *testing.T) {
	t.Parallel()
	embed := discordEmbed(t, Args{
		Level:   "info",
		Summary: "s",
		Details: map[string]any{"log": strings.Repeat("y", 4000), "": ""},
	})
	fields, _ := embed["fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("fields = %v, want two", fields)
	}
	// Discord rejects an embed field with an empty name or value; the
	// sorted-first key here is "" with an empty value.
	blank, _ := fields[0].(map[string]any)
	if blank["name"] == "" || blank["value"] == "" {
		t.Errorf("fields[0] = %v, want a placeholder rather than an empty name/value", blank)
	}
	long, _ := fields[1].(map[string]any)
	if n := utf8.RuneCountInString(long["value"].(string)); n != discordFieldValueMax {
		t.Errorf("value length = %d, want %d", n, discordFieldValueMax)
	}
}

func TestDiscord_TooManyFieldsAreCappedAndNamed(t *testing.T) {
	t.Parallel()
	details := map[string]any{}
	for i := range 40 {
		details[fmt.Sprintf("k%02d", i)] = i
	}
	embed := discordEmbed(t, Args{Level: "info", Summary: "s", Details: details})
	fields, _ := embed["fields"].([]any)
	if len(fields) != discordMaxFields+1 {
		t.Fatalf("fields = %d, want %d details plus the omission note", len(fields), discordMaxFields)
	}
	note, _ := fields[len(fields)-1].(map[string]any)
	if !strings.Contains(note["value"].(string), "15 more detail(s) omitted") {
		t.Errorf("overflow note = %v, want the omitted count", note)
	}
}

// TestDiscord_StaysUnderTheTotalCharacterBudget covers the limit that is
// easiest to miss: 25 fields are allowed, but the embed as a whole caps
// at 6000 characters, so 25 long values are a 400 even though the field
// count is legal.
func TestDiscord_StaysUnderTheTotalCharacterBudget(t *testing.T) {
	t.Parallel()
	details := map[string]any{}
	for i := range 20 {
		details[fmt.Sprintf("k%02d", i)] = strings.Repeat("w", 900)
	}
	embed := discordEmbed(t, Args{Level: "critical", Summary: "s", Details: details})
	total := utf8.RuneCountInString(embed["title"].(string))
	if d, ok := embed["description"].(string); ok {
		total += utf8.RuneCountInString(d)
	}
	fields, _ := embed["fields"].([]any)
	for _, f := range fields {
		m, _ := f.(map[string]any)
		total += utf8.RuneCountInString(m["name"].(string)) + utf8.RuneCountInString(m["value"].(string))
	}
	if total > discordEmbedTotalMax {
		t.Errorf("embed is %d characters, want at most Discord's %d", total, discordEmbedTotalMax)
	}
	last, _ := fields[len(fields)-1].(map[string]any)
	if !strings.Contains(last["value"].(string), "more detail(s) omitted") {
		t.Errorf("last field = %v, want the truncation named", last)
	}
}
