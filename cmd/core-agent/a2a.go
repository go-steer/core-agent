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
	"context"
	"fmt"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/attach"
	"github.com/go-steer/core-agent/v3/pkg/compose"
	"github.com/go-steer/core-agent/v3/pkg/config"
)

// buildDaemonA2AOptions constructs the attach.A2AOptions and session-backed
// compose.A2ABackend when cfg.A2A.Expose is true (#1407). Returns the zero
// attach.A2AOptions when A2A exposure is disabled.
func buildDaemonA2AOptions(
	ctx context.Context,
	cfg *config.Config,
	reg *attach.SessionRegistry,
	factoryDeps *compose.SessionFactoryDeps,
) attach.A2AOptions {
	if cfg == nil || !cfg.A2A.Expose {
		return attach.A2AOptions{}
	}
	backend := compose.NewA2ABackend(compose.A2ABackendOptions{
		DaemonCtx:   ctx,
		Registry:    reg,
		FactoryDeps: factoryDeps,
		EnforceACL:  cfg.Attach.MultiSession.Enabled,
	})
	return compose.BuildA2AOptions(cfg, backend)
}

// resolveAttachServerTuning parses the attach listener's session_idle_timeout,
// cost_rate_limit, and shutdown_timeout fields from cfg.Attach.
func resolveAttachServerTuning(cfg *config.Config) (time.Duration, attach.CostRateLimit, time.Duration, error) {
	var sessionIdleTimeout time.Duration
	if cfg.Attach.MultiSession.Enabled {
		raw := cfg.Attach.MultiSession.SessionIdleTimeout
		switch raw {
		case "":
			sessionIdleTimeout = 24 * time.Hour
		default:
			d, err := time.ParseDuration(raw)
			if err != nil {
				return 0, attach.CostRateLimit{}, 0, fmt.Errorf("parse session_idle_timeout=%q: %w", raw, err)
			}
			sessionIdleTimeout = d
		}
	}

	var costLimit attach.CostRateLimit
	if rl := cfg.Attach.CostRateLimit; rl != nil {
		costLimit = attach.CostRateLimit{
			PerMinute: rl.PerMinute,
			Burst:     rl.Burst,
			Disabled:  rl.Disabled,
		}
	}

	var attachShutdownTimeout time.Duration
	if raw := cfg.Attach.ShutdownTimeout; raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, attach.CostRateLimit{}, 0, fmt.Errorf("parse attach.shutdown_timeout=%q: %w", raw, err)
		}
		if d <= 0 {
			return 0, attach.CostRateLimit{}, 0, fmt.Errorf("attach.shutdown_timeout=%q: must be > 0 (omit the field to keep the 5s default)", raw)
		}
		attachShutdownTimeout = d
	}

	return sessionIdleTimeout, costLimit, attachShutdownTimeout, nil
}
