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

package eventlog

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// bootLogRow is the GORM model for the agent_boot_log table — one row
// per daemon boot that ran the auto-continue boot scan (#539,
// docs/auto-continue-design.md §crash-loop breaker). Lives in the
// eventlog database (rather than a file) so it survives pod
// replacement and reads identically for every daemon sharing the DB.
type bootLogRow struct {
	ID        uint      `gorm:"primaryKey;autoIncrement"`
	BootAt    time.Time `gorm:"index"`
	Attempted string    // JSON array of session IDs the scan tried to continue
}

func (bootLogRow) TableName() string { return "agent_boot_log" }

// BootRecord is the public view of one boot-scan run.
type BootRecord struct {
	BootAt    time.Time
	Attempted []string
}

// RecordBoot appends a boot-scan record. attempted lists the session
// IDs the scan triggered continuations for (empty slice = scan ran,
// found nothing). AutoMigrate is idempotent and cheap at once-per-boot
// call frequency.
func (h *Handle) RecordBoot(ctx context.Context, at time.Time, attempted []string) error {
	if h == nil || h.DB == nil {
		return fmt.Errorf("eventlog: RecordBoot: no database")
	}
	if err := h.DB.WithContext(ctx).AutoMigrate(&bootLogRow{}); err != nil {
		return fmt.Errorf("eventlog: migrate agent_boot_log: %w", err)
	}
	if attempted == nil {
		attempted = []string{}
	}
	blob, err := json.Marshal(attempted)
	if err != nil {
		return fmt.Errorf("eventlog: marshal attempted sessions: %w", err)
	}
	row := bootLogRow{BootAt: at, Attempted: string(blob)}
	if err := h.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("eventlog: record boot: %w", err)
	}
	return nil
}

// RecentBoots returns boot-scan records with BootAt >= since, oldest
// first. Rows with unparsable Attempted blobs are returned with a nil
// slice rather than dropped — the breaker counts boots, and losing a
// row to corruption would weaken the guard exactly when things are
// already going wrong.
func (h *Handle) RecentBoots(ctx context.Context, since time.Time) ([]BootRecord, error) {
	if h == nil || h.DB == nil {
		return nil, fmt.Errorf("eventlog: RecentBoots: no database")
	}
	if err := h.DB.WithContext(ctx).AutoMigrate(&bootLogRow{}); err != nil {
		return nil, fmt.Errorf("eventlog: migrate agent_boot_log: %w", err)
	}
	var rows []bootLogRow
	if err := h.DB.WithContext(ctx).Where("boot_at >= ?", since).Order("boot_at asc").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("eventlog: recent boots: %w", err)
	}
	out := make([]BootRecord, 0, len(rows))
	for _, r := range rows {
		rec := BootRecord{BootAt: r.BootAt}
		_ = json.Unmarshal([]byte(r.Attempted), &rec.Attempted)
		out = append(out, rec)
	}
	return out, nil
}
