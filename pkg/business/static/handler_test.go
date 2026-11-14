package static

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T, opts Options) http.Handler {
	t.Helper()
	base := t.TempDir()
	content := filepath.Join(base, "www")
	files := map[string]string{
		"index.html":               "<h1>home</h1>",
		"docs/index.html":          "docs",
		"app.css":                  "body{}",
		"app.css.br":               "BR",
		"app.css.gz":               "GZ",
		"assets/app.123.js":        "js",
		"404.html":                 "not here",
		".env":                     "SECRET=1",
		".git/config":              "secret",
		".well-known/security.txt": "contact",
	}
	must(t, os.MkdirAll(filepath.Join(content, "empty"), 0o755))
	for name, body := range files {
		p := filepath.Join(content, filepath.FromSlash(name))
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
	must(t, os.WriteFile(filepath.Join(base, "secret.txt"), []byte("secret"), 0o644))
	must(t, os.Symlink(filepath.Join(base, "secret.txt"), filepath.Join(content, "escape.txt")))

	root, err := os.OpenRoot(content)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	h, err := NewHandler(root, opts)
	must(t, err)
	return h
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type tcase struct {
	name       string
	method     string
	path       string
	reqHeaders map[string]string
	wantStatus int
	wantBody   string
	wantHeader map[string]string
}

func run(t *testing.T, h http.Handler, tests []tcase) {
	t.Helper()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, "/", nil)
			req.URL.Path = tc.path // bypass client-side cleaning
			for k, v := range tc.reqHeaders {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantBody != "" && rec.Body.String() != tc.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tc.wantBody)
			}
			for k, want := range tc.wantHeader {
				if got := rec.Header().Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "SECRET") {
				t.Errorf("leaked protected content")
			}
		})
	}
}

func TestHandlerDefaults(t *testing.T) {
	t.Parallel()
	h := setup(t, Options{})
	run(t, h, []tcase{
		{name: "root index", path: "/", wantStatus: 200, wantBody: "<h1>home</h1>",
			wantHeader: map[string]string{"Content-Type": "text/html; charset=utf-8"}},
		{name: "file", path: "/app.css", wantStatus: 200, wantBody: "body{}",
			wantHeader: map[string]string{"Content-Type": "text/css; charset=utf-8", "Vary": ""}},
		{name: "head", method: http.MethodHead, path: "/app.css", wantStatus: 200},
		{name: "precompressed off ignores br", path: "/app.css", reqHeaders: map[string]string{"Accept-Encoding": "br"},
			wantStatus: 200, wantBody: "body{}", wantHeader: map[string]string{"Content-Encoding": ""}},
		{name: "subdir index", path: "/docs/", wantStatus: 200, wantBody: "docs"},
		{name: "subdir redirect", path: "/docs", wantStatus: http.StatusMovedPermanently},
		{name: "index.html redirect", path: "/index.html", wantStatus: http.StatusMovedPermanently},
		{name: "missing", path: "/nope.txt", wantStatus: 404},
		{name: "no dir listing", path: "/empty/", wantStatus: 404},
		{name: "traversal", path: "/../secret.txt", wantStatus: 404},
		{name: "symlink escape", path: "/escape.txt", wantStatus: 404},
		{name: "dotfile", path: "/.env", wantStatus: 404},
		{name: "dot dir", path: "/.git/config", wantStatus: 404},
		{name: "dot dir listing", path: "/.git/", wantStatus: 404},
		{name: "well-known allowed", path: "/.well-known/security.txt", wantStatus: 200, wantBody: "contact"},
		{name: "post rejected", method: http.MethodPost, path: "/", wantStatus: http.StatusMethodNotAllowed},
	})
}

func TestHandlerHeaderRules(t *testing.T) {
	t.Parallel()
	h := setup(t, Options{
		Headers: map[string]string{"cache-control": "public, max-age=60", "X-Content-Type-Options": "nosniff"},
		Rules: []HeaderRule{
			{Match: "*.html", Headers: map[string]string{"Cache-Control": "no-cache"}},
			{Match: "/assets/*", Headers: map[string]string{"Cache-Control": "immutable"}},
		},
	})
	run(t, h, []tcase{
		{name: "global", path: "/app.css", wantStatus: 200,
			wantHeader: map[string]string{"Cache-Control": "public, max-age=60", "X-Content-Type-Options": "nosniff"}},
		{name: "html rule", path: "/docs/index.html", wantStatus: http.StatusMovedPermanently},
		{name: "html rule via dir", path: "/docs/", wantStatus: 200,
			wantHeader: map[string]string{"Cache-Control": "no-cache", "X-Content-Type-Options": "nosniff"}},
		{name: "html rule via root", path: "/", wantStatus: 200, wantHeader: map[string]string{"Cache-Control": "no-cache"}},
		{name: "path rule", path: "/assets/app.123.js", wantStatus: 200, wantHeader: map[string]string{"Cache-Control": "immutable"}},
		{name: "404 not cached", path: "/missing.css", wantStatus: 404, wantHeader: map[string]string{"Cache-Control": ""}},
	})
}

func TestHandlerPrecompressed(t *testing.T) {
	t.Parallel()
	h := setup(t, Options{Precompressed: true})
	run(t, h, []tcase{
		{name: "br preferred", path: "/app.css", reqHeaders: map[string]string{"Accept-Encoding": "gzip, deflate, br"},
			wantStatus: 200, wantBody: "BR", wantHeader: map[string]string{
				"Content-Encoding": "br", "Content-Type": "text/css; charset=utf-8", "Vary": "Accept-Encoding"}},
		{name: "gzip", path: "/app.css", reqHeaders: map[string]string{"Accept-Encoding": "gzip"},
			wantStatus: 200, wantBody: "GZ", wantHeader: map[string]string{"Content-Encoding": "gzip"}},
		{name: "br q=0 falls back to gzip", path: "/app.css", reqHeaders: map[string]string{"Accept-Encoding": "br;q=0, gzip;q=0.5"},
			wantStatus: 200, wantBody: "GZ"},
		{name: "identity", path: "/app.css", wantStatus: 200, wantBody: "body{}",
			wantHeader: map[string]string{"Content-Encoding": "", "Vary": "Accept-Encoding"}},
		{name: "no compressed sibling", path: "/docs/", reqHeaders: map[string]string{"Accept-Encoding": "br"},
			wantStatus: 200, wantBody: "docs", wantHeader: map[string]string{"Content-Encoding": ""}},
		{name: "missing still 404", path: "/nope.css", reqHeaders: map[string]string{"Accept-Encoding": "br"},
			wantStatus: 404, wantHeader: map[string]string{"Content-Encoding": ""}},
	})
}

func TestHandlerFallbackAndNotFound(t *testing.T) {
	t.Parallel()
	h := setup(t, Options{
		Fallback:     "/index.html",
		NotFoundPage: "/404.html",
		Rules:        []HeaderRule{{Match: "*.html", Headers: map[string]string{"Cache-Control": "no-cache"}}},
	})
	run(t, h, []tcase{
		{name: "existing file", path: "/app.css", wantStatus: 200, wantBody: "body{}"},
		{name: "existing dir", path: "/docs/", wantStatus: 200, wantBody: "docs"},
		{name: "dir redirect kept", path: "/docs", wantStatus: http.StatusMovedPermanently},
		{name: "spa route", path: "/users/42", wantStatus: 200, wantBody: "<h1>home</h1>",
			wantHeader: map[string]string{"Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-cache"}},
		{name: "missing asset gets 404 page", path: "/missing.js", wantStatus: 404, wantBody: "not here",
			wantHeader: map[string]string{"Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store"}},
		{name: "head 404 has no body", method: http.MethodHead, path: "/missing.js", wantStatus: 404},
		{name: "dir without index", path: "/empty/", wantStatus: 200, wantBody: "<h1>home</h1>"},
		{name: "dotfile uses 404 page", path: "/.env", wantStatus: 404, wantBody: "not here"},
		{name: "escape uses 404 page", path: "/escape.txt", wantStatus: 404, wantBody: "not here"},
	})
}

func TestNewHandlerValidatesPages(t *testing.T) {
	t.Parallel()
	root, err := os.OpenRoot(t.TempDir())
	must(t, err)
	t.Cleanup(func() { root.Close() })
	for _, opts := range []Options{
		{Fallback: "/index.html"},
		{NotFoundPage: "/404.html"},
		{Rules: []HeaderRule{{Match: "[", Headers: nil}}},
	} {
		if _, err := NewHandler(root, opts); err == nil {
			t.Errorf("expected error for %+v", opts)
		}
	}
}

func TestHandlerRange(t *testing.T) {
	t.Parallel()
	h := setup(t, Options{})
	req := httptest.NewRequest(http.MethodGet, "/app.css", nil)
	req.Header.Set("Range", "bytes=0-3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "body" {
		t.Errorf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestAcceptsEncoding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		header, token string
		want          bool
	}{
		{"gzip, br", "br", true},
		{"BR", "br", true},
		{"br;q=0", "br", false},
		{"br; q=0.000", "br", false},
		{"br;q=0.1", "br", true},
		{"brotli", "br", false},
		{"gzip", "br", false},
		{"", "gzip", false},
	}
	for _, tc := range tests {
		if got := acceptsEncoding(tc.header, tc.token); got != tc.want {
			t.Errorf("acceptsEncoding(%q, %q) = %v, want %v", tc.header, tc.token, got, tc.want)
		}
	}
}
