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

// Command webhook-sink is a local alert target that writes down every
// delivery it accepts, so a UAT rig can grade that a notification was
// delivered rather than that the daemon said it sent one.
//
// It exists for the #1116 T2 rig, which points permissions.approval_notify
// at it (#647). The daemon's "approval notification sent" log line is the
// sender's own claim; this file is the recipient's record, and it is the
// only one of the two a rig can hold the daemon to. A real Slack channel
// would prove the same thing to a human and nothing to a script.
//
//	webhook-sink --url-file F --out F.jsonl [--addr 127.0.0.1:0] [--bearer-env NAME]
//
// It listens on --addr (default: a free loopback port, so a rig never
// picks one and never collides), then writes the full hook URL to
// --url-file. That write is atomic, so a rig polling for the file never
// reads half a URL. Each accepted POST appends one JSON line to --out:
// the receive time, the path, the body (as JSON when it parses, else a
// string) and the request headers minus Authorization.
//
// With --bearer-env, a request must carry "Authorization: Bearer <value
// of that env var>" or it is refused with 401 and NOT recorded. Without
// it, any local process could count as a delivery, and the grade would
// measure the machine rather than the daemon.
//
// SIGINT/SIGTERM shut it down cleanly.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/childenv"
)

// maxBody bounds one delivery. The alert templates render a few KiB at
// most; anything near this is not a notification.
const maxBody = 1 << 20

// hookPath is the one path the sink serves. Anything else is a 404, so a
// target URL with a typo fails loudly instead of recording.
const hookPath = "/hook"

// record is one line of --out.
type record struct {
	ReceivedAt time.Time         `json:"received_at"`
	Path       string            `json:"path"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       json.RawMessage   `json:"body,omitempty"`
	BodyText   string            `json:"body_text,omitempty"`
}

// sink is the handler. Split from main so tests drive it with httptest.
type sink struct {
	mu    sync.Mutex
	out   io.Writer
	token string // empty: no auth check
	now   func() time.Time
}

func (s *sink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != hookPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.token != "" {
		want := "Bearer " + s.token
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}

	rec := record{ReceivedAt: s.now().UTC(), Path: r.URL.Path, Headers: map[string]string{}}
	for k := range r.Header {
		if http.CanonicalHeaderKey(k) == "Authorization" {
			continue
		}
		rec.Headers[k] = r.Header.Get(k)
	}
	if json.Valid(body) {
		rec.Body = body
	} else {
		rec.BodyText = string(body)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		http.Error(w, "encode: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	_, err = s.out.Write(append(line, '\n'))
	s.mu.Unlock()
	if err != nil {
		// A delivery the sink could not write down must not look
		// accepted: the sender would log success for a notification
		// the grade will never see.
		http.Error(w, "record: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeFileAtomic writes data to path via a temp file and rename.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".webhook-sink-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

type options struct {
	addr      string
	urlFile   string
	out       string
	bearerEnv string
}

// run serves until ctx is done. ready, when non-nil, receives the hook
// URL once the listener is up and the URL file is written.
func run(ctx context.Context, o options, getenv func(string) string, ready chan<- string) error {
	if o.urlFile == "" || o.out == "" {
		return errors.New("--url-file and --out are required")
	}
	token := ""
	if o.bearerEnv != "" {
		token = getenv(o.bearerEnv)
		if token == "" {
			return fmt.Errorf("--bearer-env %s is unset or empty; refusing to start without the auth it asked for", o.bearerEnv)
		}
	}
	f, err := os.OpenFile(o.out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open --out: %w", err)
	}
	defer func() { _ = f.Close() }()

	ln, err := net.Listen("tcp", o.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", o.addr, err)
	}
	url := "http://" + ln.Addr().String() + hookPath
	if err := writeFileAtomic(o.urlFile, []byte(url+"\n")); err != nil {
		_ = ln.Close()
		return fmt.Errorf("write --url-file: %w", err)
	}

	srv := &http.Server{
		Handler:           &sink{out: f, token: token, now: time.Now},
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if ready != nil {
		ready <- url
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			return err
		}
		if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func main() {
	var o options
	flag.StringVar(&o.addr, "addr", "127.0.0.1:0", "listen address; the default picks a free loopback port")
	flag.StringVar(&o.urlFile, "url-file", "", "write the hook URL here once listening (required)")
	flag.StringVar(&o.out, "out", "", "append one JSON line per accepted delivery (required)")
	flag.StringVar(&o.bearerEnv, "bearer-env", "", "require Authorization: Bearer <value of this env var>")
	flag.Parse()

	// The ingress token is read once and the variable dropped, and the
	// process made non-dumpable, so an agent with a shell on the same
	// machine cannot read it back out of /proc/<pid>/environ and forge a
	// delivery the rig would accept as the daemon's (#1201).
	getenv := os.Getenv
	if o.bearerEnv != "" {
		tok, err := childenv.Take(o.bearerEnv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "webhook-sink: warning: could not make this process non-dumpable (%v); the token is readable from /proc\n", err)
		}
		getenv = func(name string) string {
			if name == o.bearerEnv {
				return tok
			}
			return os.Getenv(name)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, o, getenv, nil)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "webhook-sink: %v\n", err)
		os.Exit(1)
	}
}
