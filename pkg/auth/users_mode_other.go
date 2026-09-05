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

//go:build !unix

package auth

import "os"

// fileGID cannot name an owning group on a non-POSIX platform, so it
// reports unknownGID and the group-read relaxation stays closed. On
// Windows the mode check does not run at all; everywhere else this
// leaves the historical 0600-or-stricter behaviour intact.
func fileGID(os.FileInfo) int { return unknownGID }
