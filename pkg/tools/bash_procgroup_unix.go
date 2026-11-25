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

//go:build unix

package tools

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// bashGroupGrace is how long the processes left in a cancelled command's
// process group get, after SIGTERM, before SIGKILL.
const bashGroupGrace = 2 * time.Second

// processGroup runs a bash command in its own process group, so a cancel
// (the tool timeout, an interrupt, the turn's context ending) reaches
// everything the command started, not only the shell: a `nohup ... &`
// child, a grandchild that redirected its output away, a foreground child
// the shell was waiting on. exec's default cancel kills the shell alone,
// and those processes kept running.
type processGroup struct {
	cmd *exec.Cmd
}

// startInOwnGroup puts cmd in a new process group and makes its cancel
// signal the whole group with SIGTERM. Call it before cmd starts.
//
// A command in its own group is a background job to a controlling
// terminal, if core-agent has one: a process in it that reads /dev/tty
// stops (SIGTTIN) until the command is cancelled. Its stdin is not the
// terminal either way.
func startInOwnGroup(cmd *exec.Cmd) *processGroup {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	g := &processGroup{cmd: cmd}
	cmd.Cancel = g.cancel
	return g
}

// cancel is the command's Cancel: SIGTERM to the group, and SIGCONT so a
// stopped member acts on it. If the shell has already exited and been
// reaped, it reports os.ErrProcessDone, as exec's default Kill does, so a
// command that finished just as its context ended keeps its own result;
// finishCancel still clears what the shell left.
func (g *processGroup) cancel() error {
	if err := g.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		return os.ErrProcessDone
	}
	err := g.signal(syscall.SIGTERM)
	_ = g.signal(syscall.SIGCONT)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// signal sends sig to the command's process group, whose id is the shell's
// pid (Setpgid with Pgid 0).
func (g *processGroup) signal(sig syscall.Signal) error {
	if g.cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-g.cmd.Process.Pid, sig)
}

// finishCancel runs after a cancelled command's Wait returns. Cancel may
// not have run (the shell had exited, Wait was only waiting out WaitDelay
// on pipes a background child held), so the group gets SIGTERM and SIGCONT
// here too, then bashGroupGrace to exit, then SIGKILL. A process can leave
// the group only by calling setsid or setpgid itself; those are out of
// reach here.
//
// The poll counts a zombie as a member, so where nothing reaps orphans
// (core-agent as a container's pid 1 with no init), a cancelled call
// waits the whole grace before the SIGKILL, which a zombie ignores.
func (g *processGroup) finishCancel() {
	if g.cmd.Process == nil {
		return
	}
	if g.signal(syscall.SIGTERM) != nil {
		return // the group is empty
	}
	_ = g.signal(syscall.SIGCONT)
	for deadline := time.Now().Add(bashGroupGrace); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if g.signal(0) != nil {
			return // the group is empty
		}
	}
	_ = g.signal(syscall.SIGKILL)
}
