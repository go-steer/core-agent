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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixed = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func newSink(token string) (*sink, *bytes.Buffer) {
	var buf bytes.Buffer
	return &sink{out: &buf, token: token, now: func() time.Time { return fixed }}, &buf
}

func post(t *testing.T, h http.Handler, path, auth, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

func records(t *testing.T, buf *bytes.Buffer) []record {
	t.Helper()
	var out []record
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var r record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad JSONL line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func TestAcceptedDeliveryIsRecordedWithoutTheCredential(t *testing.T) {
	s, buf := newSink("s3cret")
	body := `{"level":"warning","summary":"x","details":{"request_id":"p-1"}}`
	if code := post(t, s, hookPath, "Bearer s3cret", body); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	recs := records(t, buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if !r.ReceivedAt.Equal(fixed) || r.Path != hookPath {
		t.Errorf("record = %+v", r)
	}
	var got map[string]any
	if err := json.Unmarshal(r.Body, &got); err != nil {
		t.Fatalf("body not kept as JSON: %v", err)
	}
	if got["details"].(map[string]any)["request_id"] != "p-1" {
		t.Errorf("body = %s", r.Body)
	}
	if _, ok := r.Headers["Authorization"]; ok {
		t.Error("the Authorization header was written to the record")
	}
	if strings.Contains(buf.String(), "s3cret") {
		t.Error("the bearer token appears in the output file")
	}
	if r.Headers["Content-Type"] != "application/json" {
		t.Errorf("headers = %v", r.Headers)
	}
}

func TestRefusedDeliveriesAreNotRecorded(t *testing.T) {
	cases := []struct {
		name, path, auth string
		want             int
	}{
		{"no token", hookPath, "", http.StatusUnauthorized},
		{"wrong token", hookPath, "Bearer nope", http.StatusUnauthorized},
		{"token without scheme", hookPath, "s3cret", http.StatusUnauthorized},
		{"wrong path", "/hooks", "Bearer s3cret", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, buf := newSink("s3cret")
			if code := post(t, s, c.path, c.auth, `{}`); code != c.want {
				t.Fatalf("status = %d, want %d", code, c.want)
			}
			if buf.Len() != 0 {
				t.Fatalf("a refused delivery was recorded: %q", buf.String())
			}
		})
	}
}

func TestOnlyPostIsAccepted(t *testing.T) {
	s, buf := newSink("")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, hookPath, nil))
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET: status %d Allow %q", rr.Code, rr.Header().Get("Allow"))
	}
	if buf.Len() != 0 {
		t.Fatal("a GET was recorded")
	}
}

func TestNonJSONBodyIsKeptAsText(t *testing.T) {
	s, buf := newSink("")
	if code := post(t, s, hookPath, "", "plain words"); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	r := records(t, buf)[0]
	if r.Body != nil || r.BodyText != "plain words" {
		t.Fatalf("record = %+v", r)
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	s, buf := newSink("")
	if code := post(t, s, hookPath, "", strings.Repeat("x", maxBody+1)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", code)
	}
	if buf.Len() != 0 {
		t.Fatal("an oversized body was recorded")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// A delivery the sink could not write down must not be acknowledged, or
// the daemon logs success for a notification the grade never sees.
func TestUnrecordableDeliveryIsNotAcknowledged(t *testing.T) {
	s := &sink{out: failWriter{}, now: time.Now}
	if code := post(t, s, hookPath, "", `{}`); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
}

func TestRunRefusesAnUnsetBearerEnv(t *testing.T) {
	dir := t.TempDir()
	err := run(context.Background(), options{
		addr: "127.0.0.1:0", urlFile: filepath.Join(dir, "url"), out: filepath.Join(dir, "out"),
		bearerEnv: "SINK_TOKEN",
	}, func(string) string { return "" }, nil)
	if err == nil || !strings.Contains(err.Error(), "SINK_TOKEN") {
		t.Fatalf("err = %v, want a refusal naming SINK_TOKEN", err)
	}
}

// End to end over a real socket: the URL file names a live hook, an
// authorized POST lands in --out, and cancel shuts the server down.
func TestRunServesAndWritesTheURLFile(t *testing.T) {
	dir := t.TempDir()
	o := options{
		addr: "127.0.0.1:0", urlFile: filepath.Join(dir, "url"), out: filepath.Join(dir, "out.jsonl"),
		bearerEnv: "SINK_TOKEN",
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, o, func(k string) string {
			if k == "SINK_TOKEN" {
				return "tok"
			}
			return ""
		}, ready)
	}()

	var url string
	select {
	case url = <-ready:
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("sink never became ready")
	}
	fileURL, err := os.ReadFile(o.urlFile)
	if err != nil || strings.TrimSpace(string(fileURL)) != url {
		t.Fatalf("url file = %q (%v), want %q", fileURL, err, url)
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") || !strings.HasSuffix(url, hookPath) {
		t.Fatalf("url = %q", url)
	}

	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"summary":"hi"}`))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v after cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not shut down")
	}
	out, err := os.ReadFile(o.out)
	if err != nil || !strings.Contains(string(out), `"summary":"hi"`) {
		t.Fatalf("out = %q (%v)", out, err)
	}
}
