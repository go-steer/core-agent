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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// jsonRPCErrorBodyMax caps how much of a non-2xx response body we
// buffer while looking for a JSON-RPC error payload. Real payloads
// from Google's MCP surface are well under 4 KiB; the cap keeps a
// misbehaving server from wedging us on a large response.
const jsonRPCErrorBodyMax = 32 * 1024

// jsonRPCErrorTransport wraps an http.RoundTripper so that non-2xx
// responses whose body is a JSON-RPC-shaped payload surface the
// server's error text to the caller instead of the bare HTTP status.
//
// The upstream MCP SDK reports non-2xx responses using only
// http.StatusText(resp.StatusCode) — the JSON-RPC error body (where
// servers like Google's put the actual reason, e.g. an IAM permission
// name) is dropped. That turns actionable server errors into opaque
// "Forbidden" / "Unauthorized" strings at the operator's TUI.
//
// When the wrapped transport sees a 4xx/5xx response with an
// extractable error text, it drains the body and returns
//
//	<HTTP status>: <extracted text>
//
// as an error from RoundTrip. The SDK then wraps this as
// jsonrpc2.ErrRejected, which propagates the text to the caller
// without tearing down the underlying session. Responses whose body
// is not a recognisable JSON-RPC error are passed through unchanged
// so the SDK's own retry/session-teardown logic still runs.
type jsonRPCErrorTransport struct {
	base http.RoundTripper
}

func (t *jsonRPCErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode < 400 {
		return resp, nil
	}
	if !isJSONContentType(resp.Header.Get("Content-Type")) {
		return resp, nil
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, jsonRPCErrorBodyMax+1))
	_ = resp.Body.Close()
	if readErr != nil {
		// Body drained partially; without a full read we can't reliably
		// extract, so surface a status-only error rather than replay a
		// truncated body.
		return nil, fmt.Errorf("%s: %s", resp.Status, readErr)
	}
	if len(body) > jsonRPCErrorBodyMax {
		// Oversized body: pass through with a fresh body so the SDK
		// still sees a usable response and applies its own logic.
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	hint := extractJSONRPCError(body)
	if hint == "" {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	return nil, fmt.Errorf("%s: %s", resp.Status, hint)
}

func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	// Content-Type may include parameters (charset, boundary, ...).
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	return ct == "application/json" || strings.HasSuffix(ct, "+json")
}

// extractJSONRPCError pulls a human-readable error message out of a
// JSON-RPC-shaped response body. Recognises both shapes seen in the
// wild:
//
//   - Standard JSON-RPC error object: {"error": {"code": .., "message": ..}}
//   - MCP-tool error result: {"result": {"isError": true, "content": [{"type":"text","text":".."}]}}
//     (Google's container.googleapis.com/mcp uses this for IAM denials.)
//
// Returns "" when neither shape yields usable text.
func extractJSONRPCError(body []byte) string {
	var env struct {
		Error *struct {
			Message string `json:"message"`
			Data    any    `json:"data,omitempty"`
		} `json:"error,omitempty"`
		Result *struct {
			IsError bool `json:"isError,omitempty"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content,omitempty"`
		} `json:"result,omitempty"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	if env.Error != nil {
		msg := strings.TrimSpace(env.Error.Message)
		if msg != "" {
			return msg
		}
	}
	if env.Result != nil && env.Result.IsError {
		for _, c := range env.Result.Content {
			if c.Type == "text" || c.Type == "" {
				msg := strings.TrimSpace(c.Text)
				if msg != "" {
					return msg
				}
			}
		}
	}
	return ""
}
