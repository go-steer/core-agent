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
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ElicitorFn is the host-supplied bridge that turns a server's
// elicitation request into a user response. Interactive hosts plug in
// a function that opens a prompt and blocks; headless callers pass nil
// so the MCP client omits the elicitation capability during initialize
// instead of advertising interactive prompting it cannot serve.
//
// The implementation must respect ctx — if it returns ctx.Err the SDK
// translates that into a protocol-level cancel.
type ElicitorFn func(ctx context.Context, serverName string, req *mcp.ElicitRequest) (*mcp.ElicitResult, error)

// handlerFor wraps a non-nil host elicitor into the SDK's
// ElicitationHandler shape. When elicitor is nil it returns nil so the
// MCP client omits the elicitation capability in its initialize
// handshake and rejects any unsolicited server elicitation immediately
// instead of blocking on an interactive prompt.
func handlerFor(serverName string, elicitor ElicitorFn) func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	if elicitor == nil {
		return nil
	}
	return func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return elicitor(ctx, serverName, req)
	}
}

// DeclineHandler returns an MCP elicitation handler that declines every
// request and emits a one-line notice through send. Hosts that
// deliberately want to advertise elicitation and decline every request
// with a notice can wrap this into an ElicitorFn; headless callers
// normally pass a nil elicitor to Build so elicitation is not advertised
// at all.
func DeclineHandler(serverName string, send func(string)) func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	return func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		message := elicitationMessage(req)
		if send != nil {
			send(fmt.Sprintf("MCP server %q requested elicitation (%s); declined (no interactive UI)", serverName, message))
		}
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
}

func elicitationMessage(req *mcp.ElicitRequest) string {
	if req == nil || req.Params == nil {
		return "no details"
	}
	if msg := req.Params.Message; msg != "" {
		return "message: " + msg
	}
	return "schema-driven request"
}
