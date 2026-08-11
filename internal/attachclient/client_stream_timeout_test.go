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

package attachclient

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestStreamClientHasResponseHeaderTimeout guards the wiring: the SSE
// client must carry a response-header (time-to-first-byte) deadline so a
// wedged daemon can't hang the reconnect loop, while the RPC client
// relies on its whole-request Timeout instead and leaves the header
// timeout unset.
func TestStreamClientHasResponseHeaderTimeout(t *testing.T) {
	parsed, err := ParseURL("http://127.0.0.1:7777")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	c := NewWithCredentials(parsed, BearerCreds{}, 0)

	streamTr, ok := c.streamHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("streamHTTP.Transport is %T, want *http.Transport", c.streamHTTP.Transport)
	}
	if got := streamTr.ResponseHeaderTimeout; got != streamResponseHeaderTimeout {
		t.Errorf("streamHTTP ResponseHeaderTimeout = %v, want %v", got, streamResponseHeaderTimeout)
	}
	if c.streamHTTP.Timeout != 0 {
		t.Errorf("streamHTTP whole-request Timeout = %v, want 0 (SSE is long-lived)", c.streamHTTP.Timeout)
	}

	rpcTr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("http.Transport is %T, want *http.Transport", c.http.Transport)
	}
	if rpcTr.ResponseHeaderTimeout != 0 {
		t.Errorf("RPC client ResponseHeaderTimeout = %v, want 0 (bounded by whole-request Timeout)", rpcTr.ResponseHeaderTimeout)
	}
}

// TestUnixClientDisablesProxy guards against a regression from cloning
// http.DefaultTransport: the clone carries Proxy: ProxyFromEnvironment,
// which under a global HTTP_PROXY/ALL_PROXY would reroute (or, for a
// socks5:// proxy, break) a unix-socket dial that can't be proxied. The
// unix branch must null out Proxy; the TCP branch must keep the stock
// proxy behavior.
func TestUnixClientDisablesProxy(t *testing.T) {
	unixParsed, err := ParseURL("unix:///tmp/core-agent.sock")
	if err != nil {
		t.Fatalf("ParseURL(unix): %v", err)
	}
	unixTr, ok := newHTTPClient(unixParsed, 0, 0).Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unix Transport is %T, want *http.Transport", newHTTPClient(unixParsed, 0, 0).Transport)
	}
	if unixTr.Proxy != nil {
		t.Error("unix client Transport.Proxy is non-nil — a socket dial must not be proxied")
	}
	if unixTr.DialContext == nil {
		t.Error("unix client Transport.DialContext is nil — socket dialer not wired")
	}

	tcpParsed, err := ParseURL("http://127.0.0.1:7777")
	if err != nil {
		t.Fatalf("ParseURL(http): %v", err)
	}
	tcpTr, ok := newHTTPClient(tcpParsed, 0, 0).Transport.(*http.Transport)
	if !ok {
		t.Fatalf("tcp Transport is %T, want *http.Transport", newHTTPClient(tcpParsed, 0, 0).Transport)
	}
	if tcpTr.Proxy == nil {
		t.Error("tcp client Transport.Proxy is nil — should inherit stock ProxyFromEnvironment")
	}
}

// TestResponseHeaderTimeoutFiresOnStalledServer is the behavioral guard
// for the "daemon accepts the connection but never responds" wedge
// (bytes stuck unread in the daemon's receive queue). A client built
// with a short response-header timeout must return an error promptly
// instead of blocking forever, even with no whole-request Timeout set.
func TestResponseHeaderTimeoutFiresOnStalledServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Accept connections and hold them open without ever writing a
	// response — the stalled-daemon failure mode.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Read (and discard) the request bytes so they don't sit in
			// our receive queue, but never reply. Close on test teardown
			// via the listener close unblocking Accept.
			go func(c net.Conn) {
				buf := make([]byte, 512)
				_, _ = c.Read(buf)
				<-done
				_ = c.Close()
			}(conn)
		}
	}()

	parsed, err := ParseURL("http://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	// Short header timeout, no whole-request timeout (mirrors the SSE
	// client's config, just faster for the test).
	client := newHTTPClient(parsed, 0, 150*time.Millisecond)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, parsed.BaseURL+"/sessions/x/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected an error from the stalled server, got a response")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Do() took %v — response-header timeout did not fire (want ~150ms)", elapsed)
	}
}
