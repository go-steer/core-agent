// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// elicitMode mirrors the two MCP elicitation modes the spec defines.
// Anything not "url" is treated as form (matches the "if unset, will
// be inferred" wording in the SDK).
type elicitMode int

const (
	elicitForm elicitMode = iota
	elicitURL
)

// elicitFieldKind classifies one form field. The MCP spec restricts
// elicitation schemas to flat objects with primitive properties, so
// these are the only kinds we ever render.
type elicitFieldKind int

const (
	fieldString elicitFieldKind = iota
	fieldEnum                   // string with an enum constraint
	fieldNumber                 // floating-point
	fieldInteger
	fieldBoolean
)

// elicitField holds one property's render + input state.
//
// For string / number / integer we use bubbles' textinput so the user
// gets cursor positioning + blink for free. For enum / boolean we use
// a simple integer cursor over Choices.
type elicitField struct {
	Name        string
	Kind        elicitFieldKind
	Required    bool
	Description string

	// String / number / integer:
	input textinput.Model

	// Enum / boolean:
	Choices []string
	choice  int
}

// elicitState is the open elicitation modal's state. Non-nil on the
// Model while the dialog is up; key handling intercepts Tab / Esc /
// Enter / arrows. Out is the buffered channel the MCP elicitation
// handler is blocked on; we always send exactly one ElicitResult and
// then clear pendingElicit.
type elicitState struct {
	Mode       elicitMode
	ServerName string
	Message    string

	// Form fields (sorted alphabetically by Name for deterministic order).
	Fields []elicitField

	// URL-mode payload.
	URL string

	// Active field index for form mode (ignored for URL mode).
	Active int

	// Last validation error, surfaced under the form.
	Err string

	Out chan *mcpsdk.ElicitResult
}

// newElicitState parses an MCP elicitation request into render state.
// Returns the state plus a non-empty error if the server's request
// can't be represented (e.g. nested object schema, unsupported types);
// the caller should auto-decline in that case.
func newElicitState(serverName string, req *mcpsdk.ElicitRequest, out chan *mcpsdk.ElicitResult) (*elicitState, error) {
	if req == nil || req.Params == nil {
		return nil, errors.New("elicit: empty request")
	}
	mode := inferMode(req.Params)

	st := &elicitState{
		ServerName: serverName,
		Message:    req.Params.Message,
		Mode:       mode,
		URL:        req.Params.URL,
		Out:        out,
	}
	if mode == elicitURL {
		if st.URL == "" {
			return nil, errors.New("elicit: URL mode but no URL provided")
		}
		return st, nil
	}

	fields, err := parseSchema(req.Params.RequestedSchema)
	if err != nil {
		return nil, err
	}
	st.Fields = fields
	if len(st.Fields) > 0 && st.Fields[0].usesInput() {
		st.Fields[0].input.Focus()
	}
	return st, nil
}

// inferMode picks form vs URL. Per the SDK, an empty Mode means "infer
// from other fields" — URL when the URL field is set, form otherwise.
func inferMode(p *mcpsdk.ElicitParams) elicitMode {
	switch p.Mode {
	case "url":
		return elicitURL
	case "form":
		return elicitForm
	}
	if p.URL != "" {
		return elicitURL
	}
	return elicitForm
}

// parseSchema converts the server's RequestedSchema (a JSON Schema
// fragment, expressed as map[string]any after unmarshal) into a
// deterministic slice of elicitField.
//
// The MCP spec restricts elicitation schemas to flat top-level objects
// with primitive properties. Anything else is rejected so the host
// can auto-decline rather than render an unsafe form.
func parseSchema(rawSchema any) ([]elicitField, error) {
	if rawSchema == nil {
		return nil, errors.New("elicit: missing requestedSchema")
	}
	// Round-trip through JSON so any schema-shape value (RawMessage,
	// typed struct, map, etc.) collapses to a uniform map.
	body, err := json.Marshal(rawSchema)
	if err != nil {
		return nil, fmt.Errorf("elicit: schema marshal: %w", err)
	}
	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(body, &schema); err != nil {
		return nil, fmt.Errorf("elicit: schema parse: %w", err)
	}
	if schema.Type != "" && schema.Type != "object" {
		return nil, fmt.Errorf("elicit: unsupported schema type %q (only 'object' allowed)", schema.Type)
	}
	required := make(map[string]bool, len(schema.Required))
	for _, r := range schema.Required {
		required[r] = true
	}
	names := make([]string, 0, len(schema.Properties))
	for n := range schema.Properties {
		names = append(names, n)
	}
	sort.Strings(names)

	fields := make([]elicitField, 0, len(names))
	for _, name := range names {
		var prop struct {
			Type        string `json:"type"`
			Description string `json:"description"`
			Enum        []any  `json:"enum"`
		}
		if err := json.Unmarshal(schema.Properties[name], &prop); err != nil {
			return nil, fmt.Errorf("elicit: parse property %q: %w", name, err)
		}
		f := elicitField{
			Name:        name,
			Required:    required[name],
			Description: prop.Description,
		}
		switch {
		case len(prop.Enum) > 0:
			f.Kind = fieldEnum
			f.Choices = make([]string, 0, len(prop.Enum))
			for _, e := range prop.Enum {
				f.Choices = append(f.Choices, fmt.Sprintf("%v", e))
			}
		case prop.Type == "string":
			f.Kind = fieldString
			f.input = textinput.New()
			f.input.CharLimit = 0
		case prop.Type == "number":
			f.Kind = fieldNumber
			f.input = textinput.New()
			f.input.CharLimit = 0
		case prop.Type == "integer":
			f.Kind = fieldInteger
			f.input = textinput.New()
			f.input.CharLimit = 0
		case prop.Type == "boolean":
			f.Kind = fieldBoolean
			f.Choices = []string{"false", "true"}
		default:
			return nil, fmt.Errorf("elicit: unsupported property %q with type %q", name, prop.Type)
		}
		fields = append(fields, f)
	}
	return fields, nil
}

// nextField cycles the focus forward, wrapping at the end. Each
// transition refocuses textinputs as appropriate so the cursor blinks
// in the right place.
func (s *elicitState) nextField() { s.shiftFocus(+1) }
func (s *elicitState) prevField() { s.shiftFocus(-1) }

func (s *elicitState) shiftFocus(delta int) {
	if len(s.Fields) == 0 {
		return
	}
	if s.Fields[s.Active].usesInput() {
		s.Fields[s.Active].input.Blur()
	}
	s.Active = (s.Active + delta + len(s.Fields)) % len(s.Fields)
	if s.Fields[s.Active].usesInput() {
		s.Fields[s.Active].input.Focus()
	}
}

func (f elicitField) usesInput() bool {
	return f.Kind == fieldString || f.Kind == fieldNumber || f.Kind == fieldInteger
}

// activeUsesInput is a small wrapper for handlers that don't want to
// reach into Fields[Active]. Returns false when the form has no
// fields (defensive — Enter on an empty form just declines).
func (s *elicitState) activeUsesInput() bool {
	if len(s.Fields) == 0 {
		return false
	}
	return s.Fields[s.Active].usesInput()
}

// cycleChoice moves an enum/boolean cursor by delta with wrap.
func (s *elicitState) cycleChoice(delta int) {
	if len(s.Fields) == 0 {
		return
	}
	f := &s.Fields[s.Active]
	if len(f.Choices) == 0 {
		return
	}
	f.choice = (f.choice + delta + len(f.Choices)) % len(f.Choices)
}

// updateActiveInput passes a tea.Msg to the active textinput when one
// is focused. No-op when the active field is an enum/boolean cycler.
func (s *elicitState) updateActiveInput(msg tea.Msg) tea.Cmd {
	if len(s.Fields) == 0 {
		return nil
	}
	f := &s.Fields[s.Active]
	if !f.usesInput() {
		return nil
	}
	var cmd tea.Cmd
	f.input, cmd = f.input.Update(msg)
	return cmd
}

// validate checks each field. Returns the assembled content map plus
// "" if everything checks out, or "" + an error message describing
// the first failure.
func (s *elicitState) validate() (map[string]any, string) {
	out := map[string]any{}
	for _, f := range s.Fields {
		val, ok, err := f.value()
		if err != "" {
			return nil, fmt.Sprintf("%s: %s", f.Name, err)
		}
		if !ok {
			if f.Required {
				return nil, fmt.Sprintf("%s is required", f.Name)
			}
			continue
		}
		out[f.Name] = val
	}
	return out, ""
}

// value returns (parsed value, present, error). When err is non-empty
// the field is set but invalid. When present is false the field was
// left blank by the user.
func (f elicitField) value() (any, bool, string) {
	switch f.Kind {
	case fieldString:
		v := strings.TrimSpace(f.input.Value())
		if v == "" {
			return nil, false, ""
		}
		return v, true, ""
	case fieldNumber:
		raw := strings.TrimSpace(f.input.Value())
		if raw == "" {
			return nil, false, ""
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, true, "must be a number"
		}
		return v, true, ""
	case fieldInteger:
		raw := strings.TrimSpace(f.input.Value())
		if raw == "" {
			return nil, false, ""
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, true, "must be an integer"
		}
		return v, true, ""
	case fieldBoolean:
		// "false" → false, "true" → true. Always present (cycler always
		// shows one value), but we can treat the default ("false") as
		// user-not-set if the field is optional, leaving it out of
		// content. Simpler: always include.
		return f.choice == 1, true, ""
	case fieldEnum:
		if f.choice >= 0 && f.choice < len(f.Choices) {
			return f.Choices[f.choice], true, ""
		}
		return nil, false, ""
	}
	return nil, false, ""
}

// reply sends the result on the Out channel. The buffered channel
// guarantees this never blocks (assuming the goroutine is still
// listening, which it is unless ctx was cancelled).
func (s *elicitState) reply(action string, content map[string]any) {
	res := &mcpsdk.ElicitResult{Action: action}
	if action == "accept" {
		res.Content = content
	}
	select {
	case s.Out <- res:
	default:
		// Channel is full because the goroutine already gave up. Don't
		// block; the SDK will see ctx.Err on its end.
	}
}

// openURL is a best-effort hook to let the user press 'o' in URL mode
// to launch the URL via the system handler. Returns true on apparent
// success. Failures are silent — the user can copy the URL manually.
//
// Implementation deliberately doesn't import os/exec at the top-level
// because the form package is otherwise pure.
var openURL = func(_ context.Context, _ string) bool { return false }
