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
	"context"
	"testing"
)

// A /perms/stream handler whose channel Close has already closed returns
// and runs its deferred cleanup after Close. That cleanup must not
// close the channel a second time: it panicked the daemon on shutdown
// (#1169).
func TestUnsubscribeAfterCloseDoesNotPanic(t *testing.T) {
	t.Parallel()
	b := NewPromptBroker()
	frames, cleanup := b.Subscribe(context.Background())
	b.Close()
	if _, ok := <-frames; ok {
		t.Fatal("Close left the subscriber's channel open")
	}
	cleanup()
}
