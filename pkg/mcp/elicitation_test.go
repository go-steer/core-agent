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

package mcp

import (
	"context"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDeclineHandler_DeclinesAndNotifies(t *testing.T) {
	t.Parallel()
	var got string
	send := func(s string) { got = s }
	h := DeclineHandler("github", send)

	res, err := h(context.Background(), &mcpsdk.ElicitRequest{
		Params: &mcpsdk.ElicitParams{Message: "what's your token?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "decline" {
		t.Errorf("action = %q, want decline", res.Action)
	}
	if !strings.Contains(got, "github") || !strings.Contains(got, "what's your token?") {
		t.Errorf("notification missing detail: %q", got)
	}
}

func TestDeclineHandler_NilSendOK(t *testing.T) {
	t.Parallel()
	h := DeclineHandler("x", nil)
	if _, err := h(context.Background(), &mcpsdk.ElicitRequest{}); err != nil {
		t.Errorf("nil-send variant should be safe: %v", err)
	}
}
