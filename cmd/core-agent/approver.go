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
	"io"

	"github.com/go-steer/core-agent/v2/pkg/approver"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// wireApprover builds the approver permissions.auto configures and
// installs it on the template gate (#1175). It must run before
// DeriveForSession: sub-gates, including every daemon session's, take
// the approver from the template. No auto block wires nothing.
func wireApprover(ctx context.Context, provider models.Provider, cfg *config.Config, agentsDir string, template *permissions.Gate, stderr io.Writer) error {
	auto := cfg.Permissions.Auto
	if auto == nil {
		return nil
	}
	a, err := approver.FromConfig(ctx, provider, cfg, agentsDir)
	if err != nil {
		return err
	}
	template.SetApprover(a, auto.InstructionsPath(agentsDir))
	if len(auto.Eligible) == 0 && len(auto.EligibleBundles) == 0 {
		fmt.Fprintln(stderr, "core-agent: permissions.auto.eligible and eligible_bundles are both empty, so the approver decides nothing and mode \"auto\" asks a person about every call, as \"ask\" does")
	}
	return nil
}
