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
	"testing"

	coretui "github.com/go-steer/core-tui/tui"
)

// The attach client starts in the overlay permission modal, like
// core-agent with an unset ui.permission_layout. core-tui's zero value
// is inline, so dropping the field would silently flip the default.
// It reads no config file, so there must be no persistence hook: the
// /permissions layout switch is session-only here.
func TestAttachOptions_PermissionLayoutOverlaySessionOnly(t *testing.T) {
	t.Parallel()
	opts := attachOptions(nil, nil, "", nil, nil, nil, nil, coretui.Branding{})
	if opts.PermissionLayout != coretui.PermissionOverlay {
		t.Errorf("PermissionLayout = %v, want PermissionOverlay", opts.PermissionLayout)
	}
	if opts.PersistPermissionLayout != nil {
		t.Error("PersistPermissionLayout is set, but this client has no config file to write")
	}
}

// --no-mouse and --theme reach Options unchanged.
func TestAttachOptions_PassesFlagsThrough(t *testing.T) {
	t.Parallel()
	mouse := mouseOptFromFlag(true)
	opts := attachOptions(nil, nil, coretui.ThemeDark, mouse, nil, nil, nil, coretui.Branding{Wordmark: "w", AgentIdentity: "id"})
	if opts.Mouse != mouse {
		t.Errorf("Mouse = %v, want the --no-mouse pointer", opts.Mouse)
	}
	if opts.ForceTheme != coretui.ThemeDark {
		t.Errorf("ForceTheme = %q, want %q", opts.ForceTheme, coretui.ThemeDark)
	}
	if opts.Branding.AgentIdentity != "id" || opts.Branding.Wordmark != "w" {
		t.Errorf("Branding = %+v, want wordmark w / identity id", opts.Branding)
	}
}
