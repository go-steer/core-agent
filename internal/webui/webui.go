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

// Package webui embeds a pinned release of the mast-web operator UI
// (github.com/go-steer/mast-web) so the agent binary can serve it
// at /ui/* on its attach listener when the --ui flag is set.
//
// The dist/ subdirectory is populated at build time by
// dev/tools/fetch-mast-web (which downloads the release tagged in
// the top-level .mast-web-version file). A .gitkeep file ensures
// the directory exists on a fresh clone so //go:embed succeeds even
// before fetch-mast-web has run; in that state HasAssets returns
// false and the --ui startup path emits a clear operator-facing
// error instead of serving an empty UI.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the embedded SPA file system rooted at dist/. Always
// succeeds — the //go:embed directive is satisfied by the .gitkeep
// placeholder when fetch-mast-web hasn't been run. Use HasAssets to
// check whether the bundle is actually populated.
func FS() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}

// HasAssets reports whether the embedded bundle contains a real SPA
// (presence of dist/index.html), as opposed to just the .gitkeep
// placeholder. Used by the --ui startup path to fail fast with a
// useful error when the build didn't populate the assets.
func HasAssets() bool {
	f, err := distFS.Open("dist/index.html")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
