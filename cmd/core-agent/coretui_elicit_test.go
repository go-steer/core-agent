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

//go:build !no_tui

package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	coretui "github.com/go-steer/core-tui/tui"
)

// stubElicitor is a coretui.Elicitor that returns a fixed answer.
type stubElicitor struct {
	res coretui.ElicitResult
	err error
}

func (s stubElicitor) Elicit(context.Context, string, coretui.ElicitRequest) (coretui.ElicitResult, error) {
	return s.res, s.err
}

// oneStringSchema is the smallest request translateMCPSchemaToElicitRequest
// accepts, so these tests exercise the Elicit return path rather than
// tripping the translator's own decline.
func oneStringSchema() *mcpsdk.ElicitParams {
	return &mcpsdk.ElicitParams{
		Message: "which region?",
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"region": map[string]any{"type": "string"},
			},
		},
	}
}

// An unrenderable request must reach the calling MCP server as a
// protocol-level decline, not as a JSON-RPC error.
//
// core-tui v0.21.0 moved "I cannot draw this" out of the Action enum
// and into the error return (ErrElicitUnsupported + a placeholder
// ElicitActionCancel). Before that, the same request produced a plain
// decline with a nil error. Forwarding the new error verbatim would
// silently convert a clean answer into a server-side failure, so this
// pins the wire value.
func TestCoreMCPElicitor_UnsupportedBecomesDecline(t *testing.T) {
	c := &coreMCPElicitor{inner: stubElicitor{
		// The placeholder core-tui pairs with the sentinel — proving
		// we branch on err and not on Action.
		res: coretui.ElicitResult{Action: coretui.ElicitActionCancel},
		err: fmt.Errorf("%w: nested object", coretui.ErrElicitUnsupported),
	}}

	out, err := c.elicit(context.Background(), "srv", &mcpsdk.ElicitRequest{Params: oneStringSchema()})
	if err != nil {
		t.Fatalf("unsupported elicit must not surface as an error, got %v", err)
	}
	if out.Action != "decline" {
		t.Errorf("expected wire action %q, got %q", "decline", out.Action)
	}
	if out.Content != nil {
		t.Errorf("a declined request must carry no content, got %v", out.Content)
	}
}

// Every other error is a genuine failure to carry the request out and
// must still propagate — a cancelled context above all.
func TestCoreMCPElicitor_OtherErrorsPropagate(t *testing.T) {
	sentinel := errors.New("boom")
	c := &coreMCPElicitor{inner: stubElicitor{
		res: coretui.ElicitResult{Action: coretui.ElicitActionCancel},
		err: sentinel,
	}}

	out, err := c.elicit(context.Background(), "srv", &mcpsdk.ElicitRequest{Params: oneStringSchema()})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the underlying error to propagate, got %v", err)
	}
	if out.Action != "cancel" {
		t.Errorf("expected wire action %q, got %q", "cancel", out.Action)
	}
}

// The happy path is unchanged by the v0.21.0 error split.
func TestCoreMCPElicitor_SubmitCarriesValues(t *testing.T) {
	c := &coreMCPElicitor{inner: stubElicitor{
		res: coretui.ElicitResult{
			Action: coretui.ElicitActionSubmit,
			Values: map[string]any{"region": "us-central1"},
		},
	}}

	out, err := c.elicit(context.Background(), "srv", &mcpsdk.ElicitRequest{Params: oneStringSchema()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Action != "accept" {
		t.Errorf("expected wire action %q, got %q", "accept", out.Action)
	}
	if got := out.Content["region"]; got != "us-central1" {
		t.Errorf("expected submitted values forwarded, got %v", out.Content)
	}
}
