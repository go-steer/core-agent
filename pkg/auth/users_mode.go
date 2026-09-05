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
	"fmt"
	"os"
	"runtime"
)

// unknownGID is what fileGID reports on a platform that cannot name
// the owning group of a file. Treated as "not our group", so the
// permissive branch below can never open on a platform we cannot
// interrogate.
const unknownGID = -1

// checkUsersFileMode enforces the permission policy on a users file.
// Skipped on Windows, where Unix mode bits do not map cleanly.
func checkUsersFileMode(path string, info os.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	return validateUsersFileMode(path, info.Mode().Perm(), fileGID(info), processInGroup)
}

// validateUsersFileMode is the whole policy, as a pure function.
//
// The file holds bearer secrets, so the default remains 0600. The one
// relaxation is group-read, and only when this process is already a
// member of the owning group — which is not a laxity so much as a
// recognition that Kubernetes leaves no other option. `fsGroup` is the
// standard way to give a non-root pod read access to a Secret volume,
// and it unconditionally sets group-read on every file in that volume.
// A blanket `mode&0o077 != 0` rejection is therefore in direct conflict
// with the platform: mounting users.json from a Secret fails by
// construction, and every recipe pays for it with an initContainer that
// stages the file into an emptyDir just to re-chmod it.
//
// The equality is the entire security argument, so it is worth stating
// plainly. Group-read widens access to members of the owning group and
// to nobody else. When this process is one of those members, the bit
// grants no read that the reader did not already hold — it is a
// permission the daemon has by other means anyway. When it is not, the
// bit hands the credential to someone else, and that is still rejected,
// now with the gid named so the mismatch is diagnosable rather than a
// bare mode dump.
//
// Everything else stays strict, including the bits that might look
// harmless next to group-read:
//
//   - Any other-bit is rejected. "Other" is unbounded by definition.
//   - Group-write and group-execute are rejected. Neither is needed to
//     read a credential, and group-write means a co-member can swap
//     the daemon's entire user table.
//
// gid is the file's owning group, or unknownGID where the platform
// cannot report one. memberOf reports whether this process belongs to
// a given gid. Both are parameters so the cases that cannot be staged
// with chown — a foreign gid, an unreportable one — are still tested.
func validateUsersFileMode(path string, mode os.FileMode, gid int, memberOf func(int) bool) error {
	if mode&0o007 != 0 {
		return fmt.Errorf("auth: users file %q has mode %#o; world-accessible bits must be unset (0600, or 0640 owned by a group this process belongs to)", path, mode)
	}
	if mode&0o030 != 0 {
		return fmt.Errorf("auth: users file %q has mode %#o; group write/execute must be unset (0600, or 0640 owned by a group this process belongs to)", path, mode)
	}
	if mode&0o040 == 0 {
		return nil
	}
	if gid == unknownGID {
		return fmt.Errorf("auth: users file %q has mode %#o and this platform cannot report its owning group; use 0600", path, mode)
	}
	if !memberOf(gid) {
		return fmt.Errorf("auth: users file %q has mode %#o and is group-owned by gid %d, which this process (gid %d) is not a member of; use 0600, or set the Kubernetes fsGroup to a group the container runs as", path, mode, gid, os.Getgid())
	}
	return nil
}

// processInGroup reports whether this process belongs to gid, counting
// supplementary groups as well as the primary one.
//
// Supplementary groups have to count, or the fix does not fix the case
// it exists for: a pod that sets `fsGroup` without `runAsGroup` runs
// with a primary gid of 0 and carries fsGroup supplementally, so a
// primary-gid-only check would reject the single most common manifest
// shape. The security argument is unchanged either way — membership is
// membership, and a group the process can already read through is not
// widened by saying so in the mode bits.
func processInGroup(gid int) bool {
	if gid == unknownGID {
		return false
	}
	if gid == os.Getgid() {
		return true
	}
	// Getgroups is documented as the supplementary set; on Linux it
	// may or may not include the primary gid, which is why that is
	// checked separately above rather than relied on here.
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g == gid {
			return true
		}
	}
	return false
}
