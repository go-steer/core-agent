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

package telemetry

import (
	"context"
	"strings"
	"testing"
)

func TestSetup_None(t *testing.T) {
	t.Parallel()
	shutdown, err := Setup(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown errored: %v", err)
	}
}

func TestSetup_UnknownMode(t *testing.T) {
	t.Parallel()
	_, err := Setup(context.Background(), "smoke-signals")
	if err == nil || !strings.Contains(err.Error(), "unknown mode") {
		t.Fatalf("expected unknown-mode error, got %v", err)
	}
}

func TestSetup_Console(t *testing.T) {
	t.Parallel()
	shutdown, err := Setup(context.Background(), ModeConsole)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
}
