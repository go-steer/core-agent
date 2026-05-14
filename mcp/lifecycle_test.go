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
	"os/exec"
	"testing"
	"time"
)

func TestServer_Close_NilSafe(t *testing.T) {
	t.Parallel()
	(*Server)(nil).Close()
	(&Server{}).Close()
	(&Server{cmd: exec.Command("/bin/true")}).Close()
}

func TestServer_Close_ReapsStartedProcess(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("/bin/sh", "-c", "sleep 60")
	if err := cmd.Start(); err != nil {
		t.Skipf("can't spawn child: %v", err)
	}
	srv := &Server{cmd: cmd}

	done := make(chan struct{})
	go func() {
		srv.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("Server.Close did not return within 5s")
	}
}
