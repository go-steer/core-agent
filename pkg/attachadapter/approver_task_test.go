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

package attachadapter

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/internal/testutil"
	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// #1175 decision 6, end to end over the real attach server: a POST
// /inject is the approver's task only when its caller authenticated
// directly as an identity on task_from. A relay asserting that same
// identity — the way k8s-lookout and chat gateways inject — is not,
// and neither is an identity the list does not name.
func TestInject_ApproverTaskNeedsADirectListedCaller(t *testing.T) {
	t.Parallel()
	g := permissions.New(permissions.Options{
		Mode:             permissions.ModeAuto,
		Approver:         &testutil.ApproverProbe{},
		ApprovalTimeout:  time.Minute,
		ApproverTaskFrom: []string{"alice@example.com"},
	})
	a := newEchoAgent(t, agent.WithSession("u-1175", "s-1175"), agent.WithGate(g))
	reg := attach.NewSessionRegistry()
	if _, err := reg.Register(New(a)); err != nil {
		t.Fatal(err)
	}
	srv, err := attach.NewServer(attach.Options{
		Registry: reg,
		Addr:     "127.0.0.1:0",
		Authenticator: auth.NewBearerTokenAuth([]auth.User{
			{Identity: "alice@example.com", Token: "tok_alice"},
			{Identity: "bob@example.com", Token: "tok_bob"},
			{Identity: "sa:relay", Token: "tok_relay"},
		}, nil, []string{"sa:relay"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })
	var base string
	for deadline := time.Now().Add(2 * time.Second); base == "" && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if addr := srv.Addr(); addr != "" {
			base = "http://" + addr
		}
	}
	if base == "" {
		t.Fatal("attach listener never bound")
	}

	inject := func(token, asserted, message string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, base+"/sessions/s-1175/inject", strings.NewReader(`{"message":"`+message+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if asserted != "" {
			req.Header.Set(auth.HeaderAssertedCaller, asserted)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("inject %q: status %d", message, resp.StatusCode)
		}
	}
	inject("tok_alice", "", "alice typed this")
	inject("tok_relay", "alice@example.com", "relayed under alice")
	inject("tok_bob", "", "bob typed this")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, err := range a.Run(ctx, "") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	if got := a.ApproverTask(); got != "alice typed this" {
		t.Errorf("ApproverTask() = %q, want alice's own message alone", got)
	}
}
