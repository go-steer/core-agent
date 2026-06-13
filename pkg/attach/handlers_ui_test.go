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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestUIHandler_ServesIndex(t *testing.T) {
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>mast-web</html>")},
	}
	mux := http.NewServeMux()
	mux.Handle("/ui/", http.StripPrefix("/ui", uiHandler(assets)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/ui/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "mast-web") {
		t.Fatalf("want index.html body, got %q", string(body))
	}
}

func TestUIHandler_ServesNamedAsset(t *testing.T) {
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>mast-web</html>")},
		"app.js":     &fstest.MapFile{Data: []byte("console.log('mast-web');")},
	}
	mux := http.NewServeMux()
	mux.Handle("/ui/", http.StripPrefix("/ui", uiHandler(assets)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "console.log") {
		t.Fatalf("want app.js body, got %q", string(body))
	}
}

func TestUIHandler_FallsBackToIndexForClientRoutes(t *testing.T) {
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>mast-web</html>")},
	}
	mux := http.NewServeMux()
	mux.Handle("/ui/", http.StripPrefix("/ui", uiHandler(assets)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/ui/sessions/abc-123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 (index.html fallback), got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "mast-web") {
		t.Fatalf("want index.html fallback body, got %q", string(body))
	}
}

func TestUIHandler_404sMissingNamedAsset(t *testing.T) {
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>mast-web</html>")},
	}
	mux := http.NewServeMux()
	mux.Handle("/ui/", http.StripPrefix("/ui", uiHandler(assets)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/ui/missing.png")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for missing named asset, got %d", resp.StatusCode)
	}
}
