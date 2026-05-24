// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"sort"
	"strings"
)

// paletteKind tags the source of a palette's items.
type paletteKind int

const (
	paletteSlash paletteKind = iota // shown when input starts with "/"
	paletteFile                     // shown when the word at the cursor starts with "@"
)

// paletteItem is one selectable entry. Display is what shows in the
// list; Value is the literal text inserted on selection; Hint is
// optional secondary text shown muted. IsDir flags directory entries
// in the file palette so selecting them drills into the dir instead
// of finalizing the input.
type paletteItem struct {
	Display string
	Value   string
	Hint    string
	IsDir   bool
}

// paletteState is the in-flight state of an open palette overlay.
//
// trigger is the literal character that opened the palette ("/" or "@");
// triggerPos is the byte offset of that trigger in the textarea value.
// filter is the substring typed after the trigger; the palette uses it
// to narrow items.
type paletteState struct {
	kind       paletteKind
	items      []paletteItem // already filtered + sorted
	cursor     int
	trigger    rune
	triggerPos int
	filter     string
}

// MaxPaletteRows caps how many items render at once. Bubble Tea
// viewports handle scrolling; we keep this short to stay readable.
const MaxPaletteRows = 8

// allSlashItems returns every slash command we want exposed in the
// palette. Ordering doubles as default presentation order; group by
// frequency so the most-used commands surface first when the filter
// is empty.
func allSlashItems() []paletteItem {
	return []paletteItem{
		{Display: "/help", Value: "/help", Hint: "show help"},
		{Display: "/memory", Value: "/memory", Hint: "show loaded memory files"},
		{Display: "/stats", Value: "/stats", Hint: "session token + cost breakdown"},
		{Display: "/model", Value: "/model", Hint: "open the model picker"},
		{Display: "/mcp", Value: "/mcp", Hint: "configured MCP servers + status"},
		{Display: "/skills", Value: "/skills", Hint: "discovered skill bundles"},
		{Display: "/tools", Value: "/tools", Hint: "list every tool the agent can call"},
		{Display: "/reload", Value: "/reload", Hint: "re-read .agents/ from disk"},
		{Display: "/mouse", Value: "/mouse", Hint: "toggle mouse-wheel scrolling"},
		{Display: "/permissions", Value: "/permissions", Hint: "review approvals + persist recommended allowlist"},
		{Display: "/permissions list", Value: "/permissions list", Hint: "show current allow/deny + built-in bundles"},
		{Display: "/allow", Value: "/allow ", Hint: "add a pattern (e.g. /allow bash:git *) or bundle (/allow bundle:dev_tools)"},
		{Display: "/deny", Value: "/deny ", Hint: "add a deny pattern (e.g. /deny bash:curl *)"},
		{Display: "/clear", Value: "/clear", Hint: "clear chat history"},
		{Display: "/quit", Value: "/quit", Hint: "exit core-agent"},
	}
}

// filterPaletteItems returns items whose Display contains the filter
// (case-insensitive). Exact prefix matches sort before substring matches.
func filterPaletteItems(items []paletteItem, filter string) []paletteItem {
	if filter == "" {
		return items
	}
	low := strings.ToLower(filter)
	type ranked struct {
		item paletteItem
		rank int // 0 = prefix match, 1 = substring match
	}
	out := make([]ranked, 0, len(items))
	for _, it := range items {
		ld := strings.ToLower(it.Display)
		switch {
		case strings.HasPrefix(strings.TrimPrefix(ld, "/"), low),
			strings.HasPrefix(strings.TrimPrefix(ld, "@"), low):
			out = append(out, ranked{it, 0})
		case strings.Contains(ld, low):
			out = append(out, ranked{it, 1})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].rank < out[j].rank })
	flat := make([]paletteItem, len(out))
	for i, r := range out {
		flat[i] = r.item
	}
	return flat
}

// detectPaletteTrigger inspects the current textarea value (and where
// the cursor is) to decide whether a palette should be open.
//
// Rules (deliberately conservative so the palette doesn't pop up at
// random):
//   - Slash palette: the value begins with "/" and contains no
//     whitespace before the cursor. The filter is everything between
//     the leading "/" and the cursor.
//   - File palette: the word at the cursor (text since the last
//     whitespace) starts with "@". The filter is everything after the
//     "@" up to the cursor.
//
// Returns paletteKind, triggerPos (byte offset of the trigger), filter,
// and ok=true when a palette should be active.
func detectPaletteTrigger(value string, cursorPos int) (paletteKind, int, string, bool) {
	if cursorPos < 0 || cursorPos > len(value) {
		cursorPos = len(value)
	}
	prefix := value[:cursorPos]

	// Slash palette only triggers when "/" is the very first character
	// AND no whitespace has been typed yet.
	if strings.HasPrefix(prefix, "/") && !containsWhitespace(prefix) {
		return paletteSlash, 0, prefix[1:], true
	}

	// File palette: walk back from cursor to the last whitespace; if
	// the resulting word starts with "@", we're in.
	wordStart := strings.LastIndexAny(prefix, " \t\n")
	wordStart++ // skip the whitespace itself, or 0 when none found
	if wordStart < len(prefix) && prefix[wordStart] == '@' {
		return paletteFile, wordStart, prefix[wordStart+1:], true
	}
	return 0, 0, "", false
}

func containsWhitespace(s string) bool {
	return strings.ContainsAny(s, " \t\n")
}
