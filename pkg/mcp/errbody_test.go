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
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeRT is a minimal http.RoundTripper that returns a canned response.
type fakeRT struct {
	resp *http.Response
	err  error
}

func (f *fakeRT) RoundTrip(_ *http.Request) (*http.Response, error) {
	return f.resp, f.err
}

func newResp(status int, contentType, body string) *http.Response {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestJSONRPCErrorTransport_ExtractsToolResultText(t *testing.T) {
	// Google's GKE MCP surface returns 403 with the actual IAM
	// permission name embedded in a JSON-RPC tool-result body.
	body := `{
		"id": 4,
		"jsonrpc": "2.0",
		"result": {
			"content": [{
				"text": "Permission 'mcp.googleapis.com/tools.call' denied on resource '//container.googleapis.com/mcp/projects/X' (or it may not exist).",
				"type": "text"
			}],
			"isError": true
		}
	}`
	resp := newResp(http.StatusForbidden, "application/json; charset=UTF-8", body)
	// Restate the status line because http.StatusText returns just
	// "Forbidden"; the transport uses resp.Status in the surfaced
	// error, so mimic the real Go http package's format.
	resp.Status = "403 Forbidden"

	tr := &jsonRPCErrorTransport{base: &fakeRT{resp: resp}}
	got, err := tr.RoundTrip(&http.Request{})
	if err == nil {
		t.Fatalf("expected error, got response %+v", got)
	}
	if got != nil {
		t.Fatalf("expected nil response when error extracted, got %+v", got)
	}
	if !strings.Contains(err.Error(), "403 Forbidden") {
		t.Errorf("error missing HTTP status: %v", err)
	}
	if !strings.Contains(err.Error(), "mcp.googleapis.com/tools.call") {
		t.Errorf("error missing IAM permission text: %v", err)
	}
}

func TestJSONRPCErrorTransport_ExtractsStandardErrorObject(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`
	resp := newResp(http.StatusBadRequest, "application/json", body)
	resp.Status = "400 Bad Request"

	tr := &jsonRPCErrorTransport{base: &fakeRT{resp: resp}}
	_, err := tr.RoundTrip(&http.Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Method not found") {
		t.Errorf("error missing JSON-RPC error.message: %v", err)
	}
}

func TestJSONRPCErrorTransport_PassesThroughSuccess(t *testing.T) {
	resp := newResp(http.StatusOK, "application/json", `{"result":"ok"}`)
	tr := &jsonRPCErrorTransport{base: &fakeRT{resp: resp}}
	got, err := tr.RoundTrip(&http.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != resp {
		t.Errorf("expected the exact response through, got different value")
	}
}

func TestJSONRPCErrorTransport_PassesThroughNonJSON(t *testing.T) {
	// Nginx-style HTML 502 page: no JSON body, must pass through so
	// the SDK's own transient-error handling applies.
	body := "<html><body>502 Bad Gateway</body></html>"
	resp := newResp(http.StatusBadGateway, "text/html", body)
	tr := &jsonRPCErrorTransport{base: &fakeRT{resp: resp}}
	got, err := tr.RoundTrip(&http.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected response, got nil")
	}
	// Body must still be readable (we didn't touch it).
	b, _ := io.ReadAll(got.Body)
	if string(b) != body {
		t.Errorf("body mismatch: got %q want %q", b, body)
	}
}

func TestJSONRPCErrorTransport_PassesThroughUnparseableJSON(t *testing.T) {
	// JSON content-type but body doesn't match a JSON-RPC error
	// envelope — leave it alone so the SDK sees the raw response.
	body := `{"totally":"unrelated"}`
	resp := newResp(http.StatusForbidden, "application/json", body)
	tr := &jsonRPCErrorTransport{base: &fakeRT{resp: resp}}
	got, err := tr.RoundTrip(&http.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected response, got nil")
	}
	b, _ := io.ReadAll(got.Body)
	if string(b) != body {
		t.Errorf("body altered: got %q want %q", b, body)
	}
}

func TestJSONRPCErrorTransport_OversizedBodyPassesThrough(t *testing.T) {
	// A pathological server that returns a JSON body larger than our
	// buffer cap: pass through so the SDK still sees a usable response
	// and applies its own transient-error handling.
	big := bytes.Repeat([]byte("x"), jsonRPCErrorBodyMax+16)
	body := `{"result":{"isError":true,"content":[{"type":"text","text":"` + string(big) + `"}]}}`
	resp := newResp(http.StatusInternalServerError, "application/json", body)
	tr := &jsonRPCErrorTransport{base: &fakeRT{resp: resp}}
	got, err := tr.RoundTrip(&http.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected response, got nil")
	}
	b, _ := io.ReadAll(got.Body)
	if len(b) < jsonRPCErrorBodyMax {
		t.Errorf("expected buffered body to be preserved, got %d bytes", len(b))
	}
}

func TestJSONRPCErrorTransport_PropagatesTransportError(t *testing.T) {
	sentinel := errFake("boom")
	tr := &jsonRPCErrorTransport{base: &fakeRT{err: sentinel}}
	_, err := tr.RoundTrip(&http.Request{})
	if err != sentinel {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

func TestIsJSONContentType(t *testing.T) {
	cases := map[string]bool{
		"application/json":                true,
		"application/json; charset=UTF-8": true,
		"application/vnd.api+json":        true,
		"APPLICATION/JSON":                true,
		"text/html":                       false,
		"text/plain; charset=utf-8":       false,
		"":                                false,
	}
	for ct, want := range cases {
		if got := isJSONContentType(ct); got != want {
			t.Errorf("isJSONContentType(%q) = %v, want %v", ct, got, want)
		}
	}
}
