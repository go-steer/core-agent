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

package auth

import (
	"os"
	"strings"
	"testing"
)

// TestValidateUsersFileMode covers the cases a test cannot stage on
// disk: chown to a foreign group needs privileges CI does not have, and
// there is no way to ask the filesystem for an unreportable gid. The
// relaxation this function carries is the whole security surface of
// #944, so the rows that must stay closed are enumerated rather than
// left to the one mode a temp directory happens to produce.
func TestValidateUsersFileMode(t *testing.T) {
	t.Parallel()

	const ourGID = 1000
	const foreignGID = 2000
	memberOf := func(gid int) bool { return gid == ourGID }

	cases := []struct {
		name     string
		mode     os.FileMode
		gid      int
		wantErr  bool
		errNames string // substring the message must carry
	}{
		{name: "0600 owner only", mode: 0o600, gid: ourGID},
		{name: "0400 stricter than 0600", mode: 0o400, gid: ourGID},
		{name: "0000 stricter still", mode: 0o000, gid: ourGID},
		{
			// The #944 case: what fsGroup produces.
			name: "0640 owned by our group",
			mode: 0o640, gid: ourGID,
		},
		{
			name: "0440 owned by our group", mode: 0o440, gid: ourGID,
		},
		{
			// The equality is the entire argument. Without it the
			// relaxation hands the bearer table to another group.
			name: "0640 owned by a foreign group",
			mode: 0o640, gid: foreignGID,
			wantErr: true, errNames: "gid 2000",
		},
		{
			// A platform that cannot report the gid cannot prove
			// membership, so the permissive branch stays closed.
			name: "0640 with an unreportable gid",
			mode: 0o640, gid: unknownGID,
			wantErr: true, errNames: "cannot report its owning group",
		},
		{
			// Group-write is rejected even for our own group: a
			// co-member who can write the file can swap the whole
			// user table.
			name: "0660 owned by our group",
			mode: 0o660, gid: ourGID,
			wantErr: true, errNames: "group write/execute",
		},
		{
			name: "0650 group execute owned by our group",
			mode: 0o650, gid: ourGID,
			wantErr: true, errNames: "group write/execute",
		},
		{
			// "Other" is unbounded by definition, so no membership
			// argument can be made about it at all.
			name: "0644 world readable",
			mode: 0o644, gid: ourGID,
			wantErr: true, errNames: "world-accessible",
		},
		{
			name: "0604 world readable with no group bits",
			mode: 0o604, gid: ourGID,
			wantErr: true, errNames: "world-accessible",
		},
		{
			// Checked before the group bits, so a mode that is wrong
			// in both directions reports the worse one.
			name: "0666 wrong in both directions",
			mode: 0o666, gid: ourGID,
			wantErr: true, errNames: "world-accessible",
		},
		{
			// Setuid/sticky bits are not permission bits and never
			// reach here — Perm() masks them — but the row pins that
			// the policy reads the low nine and nothing else.
			name: "0640 with our group, high bits already masked",
			mode: os.FileMode(0o640).Perm(), gid: ourGID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateUsersFileMode("/tmp/users.json", tc.mode, tc.gid, memberOf)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("mode %#o gid %d: expected an error, got nil", tc.mode, tc.gid)
				}
				if !strings.Contains(err.Error(), tc.errNames) {
					t.Errorf("error should carry %q, got: %v", tc.errNames, err)
				}
				// Whatever the reason, the operator needs the mode.
				if !strings.Contains(err.Error(), "users.json") {
					t.Errorf("error should name the file, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("mode %#o gid %d: expected acceptance, got: %v", tc.mode, tc.gid, err)
			}
		})
	}
}

// TestProcessInGroup checks the membership oracle against the process
// this test is running as, which is the only identity available to it.
func TestProcessInGroup(t *testing.T) {
	t.Parallel()

	if !processInGroup(os.Getgid()) {
		t.Errorf("processInGroup(%d) = false for our own primary gid", os.Getgid())
	}
	if processInGroup(unknownGID) {
		t.Error("processInGroup(unknownGID) = true; the permissive branch must stay closed")
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Skipf("Getgroups: %v", err)
	}
	for _, g := range groups {
		if !processInGroup(g) {
			t.Errorf("processInGroup(%d) = false for a supplementary group we belong to", g)
		}
	}
	// Find a gid we are definitely not in. Kubernetes fsGroup values
	// live well below this, so the search terminates immediately in
	// practice.
	member := map[int]bool{os.Getgid(): true}
	for _, g := range groups {
		member[g] = true
	}
	for gid := 60000; gid < 60100; gid++ {
		if member[gid] {
			continue
		}
		if processInGroup(gid) {
			t.Errorf("processInGroup(%d) = true for a group we do not belong to", gid)
		}
		return
	}
	t.Skip("no non-member gid found in the probe range")
}
