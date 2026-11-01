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

package approval

import (
	"context"
	"time"

	"google.golang.org/adk/v2/agent"
)

const notifyTimeout = 60 * time.Second

// ParkNotice is what the gate hands NotifyPark when it parks a call.
type ParkNotice struct {
	Session    string
	Invocation string
	Workload   string
	Agent      string
	Tool       string
	Key        string
	Hint       string
	ParkedAt   time.Time
}

func (g *writeGate) announcePark(ctx agent.Context, notice ParkNotice) {
	fn := g.cfg.NotifyPark
	if fn == nil {
		return
	}
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	go func() {
		defer cancel()
		fn(nctx, notice)
	}()
}
