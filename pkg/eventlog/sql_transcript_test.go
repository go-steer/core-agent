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

package eventlog_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/statestore/conformance"
)

// TestSQLTranscript_Conformance runs the backend contract against this
// package's own SQL transcript. The suite was written against the
// memory backend; passing here is what makes it a contract rather than
// a description of one implementation.
func TestSQLTranscript_Conformance(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	dsns := map[eventlog.Transcript]string{}
	conformance.Run(t, conformance.Backend{
		Open: func(t *testing.T) eventlog.Transcript {
			dsn := filepath.Join(t.TempDir(), "transcript.db")
			tr, err := eventlog.OpenSQLTranscript(context.Background(), sqlite.Open(dsn))
			if err != nil {
				t.Fatalf("OpenSQLTranscript: %v", err)
			}
			mu.Lock()
			dsns[tr] = dsn
			mu.Unlock()
			return tr
		},
		Reopen: func(t *testing.T, prev eventlog.Transcript) eventlog.Transcript {
			mu.Lock()
			dsn := dsns[prev]
			mu.Unlock()
			tr, err := eventlog.OpenSQLTranscript(context.Background(), sqlite.Open(dsn))
			if err != nil {
				t.Fatalf("OpenSQLTranscript (reopen): %v", err)
			}
			return tr
		},
	})
}
