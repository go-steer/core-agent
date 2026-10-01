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

package config

import (
	"strings"
	"testing"
)

// #1175 decision 13: the gate implements "auto", but config must not
// select it until both TUIs can display it — today they would show
// "ask" while a model approves calls. The error says it is not
// available yet, not that it is unknown.
func TestValidate_PermissionModeAutoNotYetSelectable(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	c.Permissions.Mode = "auto"
	c.Permissions.ApprovalTimeout = "5m"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "not available yet") {
		t.Fatalf("Validate() with mode auto = %v, want a not-available-yet error", err)
	}
}
