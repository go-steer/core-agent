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

//go:build linux

package childenv

import "golang.org/x/sys/unix"

// ReadPtraceCaps reports where CAP_SYS_PTRACE sits in this process's
// bounding, inheritable and ambient sets.
//
// prctl(PR_CAPBSET_READ) returns 1 when the capability is in the
// calling thread's bounding set, 0 when not. capget(2) with
// _LINUX_CAPABILITY_VERSION_3 fills two CapUserData words; capability
// 19 is in the first. Both are per thread, but nothing in core-agent
// changes them, so every thread carries the sets the process started
// with. An error from either is returned: a root daemon whose sets
// cannot be read gets the warning in its "could not read" form rather
// than silence.
//
// PR_CAP_AMBIENT_IS_SET fails with EINVAL on kernels before 4.3, which
// have no ambient set at all, so its error means "not set".
func ReadPtraceCaps() (PtraceCaps, error) {
	var c PtraceCaps
	bnd, err := unix.PrctlRetInt(unix.PR_CAPBSET_READ, unix.CAP_SYS_PTRACE, 0, 0, 0)
	if err != nil {
		return c, err
	}
	c.Bounding = bnd == 1
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return c, err
	}
	c.Inheritable = data[0].Inheritable&(1<<unix.CAP_SYS_PTRACE) != 0
	amb, err := unix.PrctlRetInt(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_IS_SET, unix.CAP_SYS_PTRACE, 0, 0)
	c.Ambient = err == nil && amb == 1
	return c, nil
}
