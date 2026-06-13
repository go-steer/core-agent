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
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// uiHandler serves a mast-web SPA bundle (or any compatible static
// asset tree) from /ui/*. Unknown paths fall back to index.html so
// client-side routes resolve; named assets (anything with a file
// extension) 404 cleanly when missing.
//
// Wired by Server when Options.UI is non-nil.
func uiHandler(assets fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(r.URL.Path)
		if clean == "/" || clean == "." || clean == "" {
			fileServer.ServeHTTP(w, r)
			return
		}
		// No extension → treat as client-side route → serve index.html.
		if path.Ext(clean) == "" {
			rel := strings.TrimPrefix(clean, "/")
			f, err := assets.Open(rel)
			if err != nil {
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/"
				fileServer.ServeHTTP(w, r2)
				return
			}
			_ = f.Close()
		}
		fileServer.ServeHTTP(w, r)
	})
}
