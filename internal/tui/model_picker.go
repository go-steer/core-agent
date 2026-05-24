// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

// modelPickerState is the overlay state for /model. items holds the
// candidate model IDs, cursor is the highlighted row.
type modelPickerState struct {
	items  []string
	cursor int
}

// availableModels returns the hardcoded list of Gemini 3.x candidate
// IDs surfaced in the /model picker. Originally discovered by
// listing publisher models on the dev Vertex project; extend this
// list as Google ships GA versions or new variants.
func availableModels() []string {
	return []string{
		// -customtools variant is the default in DefaultConfig — prefers
		// registered tools over raw bash. Same price/context/reasoning as
		// the bare variant; better behavior for coding-assistant use.
		"gemini-3.1-pro-preview-customtools",
		"gemini-3.1-pro-preview",
		"gemini-3.5-flash",
		"gemini-3-flash-preview",
		"gemini-3.1-flash-lite-preview",
		"gemini-3.1-flash-image-preview",
		// 2.5 family — kept around because some accounts still rely on them.
		"gemini-2.5-pro",
		"gemini-2.5-flash",
	}
}

// indexOfModel returns the position of id in availableModels(), or -1.
func indexOfModel(id string) int {
	for i, m := range availableModels() {
		if m == id {
			return i
		}
	}
	return -1
}
