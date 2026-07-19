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

// TestSetup_EnvVarOverridesMode pins the OTel-standard override
// convention: when OTEL_TRACES_EXPORTER is set, it wins over the
// config-file mode. Load-bearing for multi-daemon k8s deployments
// where a shared ConfigMap can't carry per-Pod exporter targets.
//
// Not t.Parallel: mutates process env + OTel global providers.
func TestSetup_EnvVarOverridesMode(t *testing.T) {
	// Config says "none"; env var says "console". Env wins.
	t.Setenv(TracesExporterEnvVar, ModeConsole)
	shutdown, err := Setup(context.Background(), ModeNone)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// Successful setup with a real console exporter means the env
	// override took effect — ModeNone would have short-circuited to
	// the noop path and never touched the exporter constructors.
	// Assert indirectly by verifying shutdown is non-nil (noop path
	// returns a shutdown that does nothing but is also non-nil, so
	// this isn't perfect; the negative assertion below catches the
	// mistake).
	if shutdown == nil {
		t.Errorf("shutdown must be non-nil regardless of path")
	}
}

// TestSetup_EmptyEnvVarLeavesConfigMode pins that an unset env var
// (or explicitly empty string) doesn't accidentally override a
// non-none config value. Env-unset ≠ "select none" — the empty case
// must fall through so config wins.
func TestSetup_EmptyEnvVarLeavesConfigMode(t *testing.T) {
	t.Setenv(TracesExporterEnvVar, "") // explicit empty
	shutdown, err := Setup(context.Background(), ModeConsole)
	if err != nil {
		t.Fatalf("Setup: %v (config-mode should have applied when env is empty)", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
}

// TestSetup_EnvVarInvalidValueSurfacesSameError pins the error
// surface: an invalid env-var value produces the same clear
// "unknown mode" error as an invalid config value — no silent
// fallthrough that could mask an operator typo in a k8s manifest.
func TestSetup_EnvVarInvalidValueSurfacesSameError(t *testing.T) {
	t.Setenv(TracesExporterEnvVar, "smoke-signals")
	_, err := Setup(context.Background(), ModeNone)
	if err == nil || !strings.Contains(err.Error(), "unknown mode") {
		t.Fatalf("expected unknown-mode error for invalid env value, got %v", err)
	}
}
