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

package memory

import (
	"context"
	"errors"
	"strings"

	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type adkServiceBridge struct {
	router           *Router
	consolidateMount string
}

// AsADKService adapts a Router to google.golang.org/adk/v2/memory.Service so
// runner.Config.MemoryService works seamlessly with third-party ADK tools that
// invoke toolCtx.SearchMemory.
//
// If r is nil, AsADKService returns nil. consolidateMount names the target
// writable mount used if a caller invokes AddSessionToMemory directly (empty
// defaults to "agent"; "off" disables AddSessionToMemory).
func AsADKService(r *Router, consolidateMount string) adkmemory.Service {
	if r == nil {
		return nil
	}
	return &adkServiceBridge{
		router:           r,
		consolidateMount: strings.TrimSpace(consolidateMount),
	}
}

func (b *adkServiceBridge) AddSessionToMemory(ctx context.Context, s session.Session) error {
	if b.consolidateMount == SubagentScopeOff {
		return ErrReadOnlyMount
	}
	_, err := b.router.ConsolidateSession(ctx, b.consolidateMount, s, nil)
	return err
}

func (b *adkServiceBridge) SearchMemory(ctx context.Context, req *adkmemory.SearchRequest) (*adkmemory.SearchResponse, error) {
	if req == nil {
		return nil, errors.New("memory: nil SearchRequest")
	}
	if b.router.userResolver(ctx) == "" && strings.TrimSpace(req.UserID) != "" {
		ctx = WithUserID(ctx, req.UserID)
	}
	items, _, err := b.router.Recall(ctx, req.Query, nil, nil, 0)
	if err != nil {
		return nil, err
	}
	entries := make([]adkmemory.Entry, 0, len(items))
	for _, it := range items {
		ts := it.UpdatedAt
		if ts.IsZero() {
			ts = it.CreatedAt
		}
		meta := make(map[string]any, len(it.Metadata)+4)
		for k, v := range it.Metadata {
			meta[k] = v
		}
		if len(it.Topics) > 0 {
			meta["topics"] = it.Topics
		}
		if it.Distance != nil {
			meta["distance"] = *it.Distance
		}
		// Stamp authoritative Router provenance last so backend Metadata keys
		// cannot spoof mount identity or read_only posture.
		meta["mount"] = it.Mount
		meta["read_only"] = it.ReadOnly
		author := it.Mount
		if author == "" {
			author = MountAgent
		}
		entries = append(entries, adkmemory.Entry{
			ID:             it.ID,
			Content:        genai.NewContentFromText(it.Fact, genai.RoleUser),
			Author:         author,
			Timestamp:      ts,
			CustomMetadata: meta,
		})
	}
	return &adkmemory.SearchResponse{Memories: entries}, nil
}
