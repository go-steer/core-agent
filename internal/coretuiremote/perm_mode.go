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

package coretuiremote

import (
	"context"
	"fmt"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// FetchPermissionMode builds Options.PermissionMode for an attached
// session (#1168): the chip starts at the daemon's live mode, and
// Shift+Tab posts /perms/mode. Returns the zero wiring — which hides
// the chip — when the daemon's mode can't be read, rather than showing
// a chip whose starting value is a guess.
//
// A refused Set (not the owner, or a pre-1.16.0 daemon) returns the
// daemon's error, and core-tui rolls the chip back and shows it.
//
// The chip is bound to this Adapter's session. core-tui wires it once
// and never rebinds it on /switch, /attach or /new, so after a hop it
// refuses instead of posting: posting to the session it was bound to
// would change a session the operator is no longer looking at, perhaps
// on another daemon, and retargeting to the viewed session would apply
// a value computed from the old session's mode. Re-attaching to the
// session binds a chip to it; so does switching back to it on the same
// daemon, which reuses this Adapter's client.
//
// The chip also follows only this client's own changes. A change made
// by another attached client, or by the daemon's local TUI, is not
// pushed to it, so it can show a stale value until it is pressed.
// Pressing it applies exactly the value it then shows, so what the
// operator sees after a press is what the daemon has.
func (a *Adapter) FetchPermissionMode(ctx context.Context) coretui.PermissionModeWiring {
	info, err := a.client.Perms(ctx, a.sessionPath)
	if err != nil || info.Mode == "" {
		return coretui.PermissionModeWiring{}
	}
	return coretui.PermissionModeWiring{
		Initial: permModeToChip(permissions.Mode(info.Mode)),
		Cycle:   chipCycle(info.SettableModes),
		Set: func(m coretui.PermissionMode) error {
			if v := a.view.get(); !v.sameSession(a) {
				return fmt.Errorf("the mode chip belongs to session %s, which this TUI is no longer showing; "+
					"re-attach to %s to change its mode", currentSessionID(a.sessionPath), currentSessionID(v.sessionPath))
			}
			_, err := a.client.SetPermMode(context.TODO(), a.sessionPath, string(chipToPermMode(m)))
			return err
		},
	}
}

// chipCycle is the Shift+Tab order from the daemon's settable_modes
// (protocol 1.18.0), so the attached chip offers exactly what
// POST /perms/mode will accept for this session — auto only when the
// session can enter it. A pre-1.18.0 daemon sends none, and nil leaves
// core-tui's default four.
func chipCycle(settable []string) []coretui.PermissionMode {
	if len(settable) == 0 {
		return nil
	}
	out := make([]coretui.PermissionMode, 0, len(settable))
	for _, s := range settable {
		m := permissions.Mode(s)
		if m != permissions.ModeAsk && permModeToChip(m) == coretui.PermissionModeDefault {
			// A mode this client has no chip for (a newer daemon's).
			// Mapped, it would land on ask's chip and post "ask".
			continue
		}
		out = append(out, permModeToChip(m))
	}
	return out
}

// permModeToChip / chipToPermMode mirror cmd/core-agent's
// translateMode / translateModeBack, so an attached chip and the local
// one agree. "allow" has no chip and shows as default, as it does
// locally; cycling out of default never re-enters it.
func permModeToChip(m permissions.Mode) coretui.PermissionMode {
	switch m {
	case permissions.ModeAuto:
		// Its own chip (#1175 decision 13). The default arm below would
		// show "ask" while a model approves calls.
		return coretui.PermissionModeAuto
	case permissions.ModeAcceptEdits:
		return coretui.PermissionModeAcceptEdits
	case permissions.ModePlan:
		return coretui.PermissionModePlan
	case permissions.ModeYolo:
		return coretui.PermissionModeBypass
	default:
		return coretui.PermissionModeDefault
	}
}

func chipToPermMode(m coretui.PermissionMode) permissions.Mode {
	switch m {
	case coretui.PermissionModeAuto:
		return permissions.ModeAuto
	case coretui.PermissionModeAcceptEdits:
		return permissions.ModeAcceptEdits
	case coretui.PermissionModePlan:
		return permissions.ModePlan
	case coretui.PermissionModeBypass:
		return permissions.ModeYolo
	default:
		return permissions.ModeAsk
	}
}

// sameSession reports whether b is attached to the same session as a
// through the same client, which /switch back to a session on the same
// daemon preserves.
func (a *Adapter) sameSession(b *Adapter) bool {
	return a == b || (a != nil && b != nil && a.client == b.client && a.sessionPath == b.sessionPath)
}
