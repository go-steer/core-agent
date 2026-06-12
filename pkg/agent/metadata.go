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

package agent

import (
	"context"

	"github.com/go-steer/core-agent/pkg/auth"
	"github.com/go-steer/core-agent/pkg/eventlog"
)

// EventlogMetadataExtractor returns an eventlog.MetadataExtractor
// that pulls the per-event auth.Caller identity (and proxy
// attribution, when present) from the request context onto the
// eventlog row's sidecar metadata.
//
// Pass to eventlog.Open via eventlog.WithMetadataExtractor — typically
// at daemon startup in cmd/core-agent. The extractor is a pure
// function of the per-event context, so the eventlog package itself
// stays auth-agnostic; only the binary that wires both packages
// together depends on both.
//
// Returns nil entries when no Caller is on context (legacy /
// single-user / out-of-band Run callers); the eventlog package then
// stores no sidecar JSON for that row.
func EventlogMetadataExtractor() eventlog.MetadataExtractor {
	return func(ctx context.Context) map[string]string {
		c, ok := auth.CallerFromContext(ctx)
		if !ok || c.Identity == "" {
			return nil
		}
		md := map[string]string{
			eventlog.MetadataKeyCaller: c.Identity,
		}
		if by, ok := auth.ProxyByFromContext(ctx); ok && by != "" {
			md[eventlog.MetadataKeyProxyBy] = by
		}
		return md
	}
}
