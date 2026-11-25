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

package compose

import (
	"context"
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

// Every session a daemon builds (POST /sessions, resume, A2A, scheduled)
// runs at model.thinking_level, not only the primary one (#1605).
func TestReproduceAgent_ThinkingLevel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"configured", &config.Config{Model: config.ModelConfig{Name: "m", ThinkingLevel: "high"}}, "high"},
		{"unset", &config.Config{Model: config.ModelConfig{Name: "m"}}, ""},
		{"no config", nil, ""},
	} {
		deps := SessionFactoryDeps{
			DaemonCtx: ctx,
			Model:     namedLLM{name: "daemon-default"},
			Template:  permissions.New(permissions.Options{}),
			Cfg:       tc.cfg,
		}
		ad, cancelSess, err := ReproduceAgent(deps, auth.Anonymous, "sid-"+tc.name, "created")
		if err != nil {
			t.Fatalf("%s: ReproduceAgent: %v", tc.name, err)
		}
		t.Cleanup(cancelSess)
		if got := ad.Agent().ThinkingLevel(); got != tc.want {
			t.Errorf("%s: session thinking level %q, want %q", tc.name, got, tc.want)
		}
	}
}
