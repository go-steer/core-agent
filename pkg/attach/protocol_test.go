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

package attach

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestProtocolMajor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"1.4.0", 1, true},
		{"1", 1, true},
		{"2.0.0", 2, true},
		{"v3.1.2", 3, true},
		{"1.4.0-rc.1", 1, true},
		{"10.0.0", 10, true},
		{"1.4.0+build.5", 1, true},
		{"", 0, false},
		{"x.y.z", 0, false},
		{"v", 0, false},
		{"-1.0.0", 0, false},
		{"latest", 0, false},
	}
	for _, tc := range cases {
		got, ok := protocolMajor(tc.in)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("protocolMajor(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestNegotiateProtocolVersion drives the negotiation helper directly
// across the four cases: no declaration (accept), matching major
// (accept), skewed major (409), and malformed (400). The response
// header must always carry the server's version.
func TestNegotiateProtocolVersion(t *testing.T) {
	t.Parallel()
	serverMajor, _ := protocolMajor(protocolVersion)
	skewed := strconv.Itoa(serverMajor+1) + ".0.0"

	cases := []struct {
		name     string
		query    string // ?protocol= value ("" = omit)
		header   string // X-Attach-Protocol-Version value ("" = omit)
		wantOK   bool
		wantCode int // only checked when wantOK is false
	}{
		{name: "no declaration", wantOK: true},
		{name: "matching exact", query: protocolVersion, wantOK: true},
		{name: "matching older minor same major", query: strconv.Itoa(serverMajor) + ".0.0", wantOK: true},
		{name: "matching via header", header: protocolVersion, wantOK: true},
		{name: "skewed major query", query: skewed, wantOK: false, wantCode: http.StatusConflict},
		{name: "skewed major header", header: skewed, wantOK: false, wantCode: http.StatusConflict},
		{name: "malformed", query: "not-a-version", wantOK: false, wantCode: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			url := "/sessions/app/sid/events"
			if tc.query != "" {
				url += "?" + queryProtocolVersion + "=" + tc.query
			}
			r := httptest.NewRequest(http.MethodGet, url, nil)
			if tc.header != "" {
				r.Header.Set(headerProtocolVersion, tc.header)
			}
			w := httptest.NewRecorder()

			ok := negotiateProtocolVersion(w, r)
			if ok != tc.wantOK {
				t.Fatalf("negotiateProtocolVersion ok = %v, want %v", ok, tc.wantOK)
			}
			// Server version always echoed on the response.
			if got := w.Header().Get(headerProtocolVersion); got != protocolVersion {
				t.Errorf("response %s = %q, want %q", headerProtocolVersion, got, protocolVersion)
			}
			if !tc.wantOK && w.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tc.wantCode)
			}
		})
	}
}

// TestQueryBeatsHeader pins that the ?protocol= query param takes
// precedence over the header when both are present.
func TestNegotiateProtocolVersion_QueryBeatsHeader(t *testing.T) {
	t.Parallel()
	serverMajor, _ := protocolMajor(protocolVersion)
	skewed := strconv.Itoa(serverMajor+1) + ".0.0"

	// Query declares a compatible version, header declares a skewed one.
	// Query wins → accept.
	r := httptest.NewRequest(http.MethodGet,
		"/sessions/app/sid/events?"+queryProtocolVersion+"="+protocolVersion, nil)
	r.Header.Set(headerProtocolVersion, skewed)
	w := httptest.NewRecorder()
	if !negotiateProtocolVersion(w, r) {
		t.Fatalf("expected accept when query declares compatible version, got reject (code %d)", w.Code)
	}
}

// TestEvents_ProtocolNegotiation_Endpoint exercises the negotiation
// through the real /events HTTP handler: a skewed-major client is
// rejected with 409 before any SSE frame flows, a compatible client
// streams normally and sees the server version echoed on the response
// header, and a malformed declaration is 400. This is the regression
// guard for #389 — before the fix the server accepted any client and
// streamed frames regardless of declared version.
func TestEvents_ProtocolNegotiation_Endpoint(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "proto", log: h}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	serverMajor, _ := protocolMajor(protocolVersion)
	skewed := strconv.Itoa(serverMajor+1) + ".0.0"

	do := func(t *testing.T, rawQueryOrHeader func(*http.Request)) *http.Response {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		t.Cleanup(cancel)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			base+"/sessions/core-agent/proto/events", nil)
		rawQueryOrHeader(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		return resp
	}

	t.Run("skewed major rejected 409", func(t *testing.T) {
		resp := do(t, func(req *http.Request) {
			req.Header.Set(headerProtocolVersion, skewed)
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 409; body=%s", resp.StatusCode, b)
		}
		if got := resp.Header.Get(headerProtocolVersion); got != protocolVersion {
			t.Errorf("response %s = %q, want %q", headerProtocolVersion, got, protocolVersion)
		}
	})

	t.Run("malformed rejected 400", func(t *testing.T) {
		resp := do(t, func(req *http.Request) {
			q := req.URL.Query()
			q.Set(queryProtocolVersion, "garbage")
			req.URL.RawQuery = q.Encode()
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, b)
		}
	})

	t.Run("compatible client streams", func(t *testing.T) {
		resp := do(t, func(req *http.Request) {
			req.Header.Set(headerProtocolVersion, protocolVersion)
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, b)
		}
		if got := resp.Header.Get(headerProtocolVersion); got != protocolVersion {
			t.Errorf("response %s = %q, want %q", headerProtocolVersion, got, protocolVersion)
		}
		// The stream should open with the capabilities frame as usual.
		frames := readSSEFrames(t, resp.Body)
		first := mustReadFrame(t, frames, time.Second, "capabilities")
		if first.Event != EventCapabilities {
			t.Fatalf("first frame = %q, want %q", first.Event, EventCapabilities)
		}
	})

	t.Run("no declaration accepted", func(t *testing.T) {
		resp := do(t, func(*http.Request) {})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, b)
		}
	})
}
