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

package attach

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// debugf writes a timestamped diagnostic line to the file named by
// CORE_AGENT_DEBUG when set, otherwise drops it silently. Same
// pattern as cmd/core-agent/debug.go so a single CORE_AGENT_DEBUG=
// path captures both daemon and attach-server output in one log.
//
// Reading an env var inside a library package is slightly grungy,
// but the alternative — plumbing a logger interface through every
// constructor — would be much more invasive for a debug hook that's
// silent by default.
func debugf(format string, args ...any) {
	w := debugWriter()
	if w == nil {
		return
	}
	fmt.Fprintf(w, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, args...)...)
}

var (
	debugOnce sync.Once
	debugFile *os.File
)

func debugWriter() *os.File {
	debugOnce.Do(func() {
		path := os.Getenv("CORE_AGENT_DEBUG")
		if path == "" {
			return
		}
		// #nosec G703 — path is operator-supplied env var; entire
		// point of this hook is for the operator to choose where
		// debug output lands.
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		debugFile = f
	})
	return debugFile
}
