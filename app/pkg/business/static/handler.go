// Package static serves files from a directory confined with os.Root.
package static

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
)

// HeaderRule adds headers to files whose path matches Match (path.Match
// syntax). A pattern without "/" matches the file's base name; otherwise it
// matches the full URL path of the file, e.g. "/assets/*".
type HeaderRule struct {
	Match   string
	Headers map[string]string
}

type Options struct {
	// Headers are added to every file response.
	Headers map[string]string
	// Rules are applied in order after Headers; later rules override earlier ones.
	Rules []HeaderRule
	// Fallback is served with 200 for missing paths without a file extension
	// (single-page apps), e.g. "/index.html".
	Fallback string
	// NotFoundPage is served with 404 for missing files, e.g. "/404.html".
	NotFoundPage string
	// Precompressed serves "<file>.br" / "<file>.gz" when the client accepts them.
	Precompressed bool
}

type rule struct {
	match   string
	headers http.Header
}

type handler struct {
	root          *os.Root
	files         http.Handler
	headers       http.Header
	rules         []rule
	fallback      string
	notFoundPage  string
	precompressed bool
}

// NewHandler serves GET/HEAD requests from root. Configured fallback and
// not-found pages must exist in root.
func NewHandler(root *os.Root, opts Options) (http.Handler, error) {
	h := &handler{
		root:          root,
		files:         http.FileServer(rootFS{root: root}),
		headers:       toHeader(opts.Headers),
		fallback:      strings.TrimPrefix(opts.Fallback, "/"),
		notFoundPage:  strings.TrimPrefix(opts.NotFoundPage, "/"),
		precompressed: opts.Precompressed,
	}
	for _, r := range opts.Rules {
		if _, err := path.Match(r.Match, ""); err != nil {
			return nil, fmt.Errorf("header rule %q: %w", r.Match, err)
		}
		h.rules = append(h.rules, rule{match: r.Match, headers: toHeader(r.Headers)})
	}
	for _, p := range []string{h.fallback, h.notFoundPage} {
		if p == "" {
			continue
		}
		if info, err := root.Stat(p); err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("page %q is not a regular file in the content root", "/"+p)
		}
	}
	return h, nil
}

func toHeader(m map[string]string) http.Header {
	h := make(http.Header, len(m))
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	upath := r.URL.Path
	if !strings.HasPrefix(upath, "/") {
		upath = "/" + upath
	}
	clean := path.Clean(upath)
	isDir := strings.HasSuffix(upath, "/")
	if hidden(clean) {
		h.notFound(w, r)
		return
	}

	// name is the file that will be served, relative to the root.
	name := strings.TrimPrefix(clean, "/")
	if isDir {
		name = path.Join(name, "index.html")
	}

	// "/index.html" is left to http.FileServer so it can redirect to "/".
	if h.precompressed && !strings.HasSuffix(upath, "/index.html") && h.serveEncoded(w, r, name) {
		return
	}

	// Existence is only checked when it changes the outcome, so the default
	// path stays a single open in http.FileServer.
	if (h.fallback != "" || h.notFoundPage != "") && !h.exists(clean) {
		if h.fallback != "" && path.Ext(clean) == "" {
			h.serveFile(w, r, h.fallback, http.StatusOK)
			return
		}
		h.notFound(w, r)
		return
	}

	h.setHeaders(w.Header(), name)
	h.files.ServeHTTP(w, r)
}

// hidden reports whether any path segment is a dotfile, except .well-known.
func hidden(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if strings.HasPrefix(seg, ".") && seg != ".well-known" {
			return true
		}
	}
	return false
}

func (h *handler) setHeaders(dst http.Header, name string) {
	for k, v := range h.headers {
		dst[k] = v
	}
	for _, rl := range h.rules {
		target := path.Base(name)
		if strings.Contains(rl.match, "/") {
			target = "/" + name
		}
		if ok, _ := path.Match(rl.match, target); ok {
			for k, v := range rl.headers {
				dst[k] = v
			}
		}
	}
	if h.precompressed {
		dst.Add("Vary", "Accept-Encoding")
	}
}

// exists mirrors what rootFS will serve: regular files, and directories that
// contain an index.html.
func (h *handler) exists(clean string) bool {
	name := strings.TrimPrefix(clean, "/")
	if name == "" {
		name = "."
	}
	info, err := h.root.Stat(name)
	if err != nil {
		return false
	}
	if info.IsDir() {
		info, err = h.root.Stat(path.Join(name, "index.html"))
		if err != nil {
			return false
		}
	}
	return info.Mode().IsRegular()
}

var encodings = [...]struct{ token, ext string }{{"br", ".br"}, {"gzip", ".gz"}}

func (h *handler) serveEncoded(w http.ResponseWriter, r *http.Request, name string) bool {
	ae := r.Header.Get("Accept-Encoding")
	if ae == "" {
		return false
	}
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		return false
	}
	for _, enc := range encodings {
		if !acceptsEncoding(ae, enc.token) {
			continue
		}
		f, info, ok := h.openRegular(name + enc.ext)
		if !ok {
			continue
		}
		defer f.Close()
		hdr := w.Header()
		h.setHeaders(hdr, name)
		hdr.Set("Content-Type", ctype)
		hdr.Set("Content-Encoding", enc.token)
		http.ServeContent(w, r, name, info.ModTime(), f)
		return true
	}
	return false
}

// acceptsEncoding reports whether an Accept-Encoding header allows token
// (q=0 means "not acceptable").
func acceptsEncoding(header, token string) bool {
	for part := range strings.SplitSeq(header, ",") {
		coding, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(coding), token) {
			continue
		}
		for p := range strings.SplitSeq(params, ";") {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			if strings.EqualFold(k, "q") {
				v = strings.TrimRight(strings.TrimSpace(v), "0")
				if v == "" || v == "0" || v == "0." {
					return false
				}
			}
		}
		return true
	}
	return false
}

func (h *handler) openRegular(name string) (*os.File, fs.FileInfo, bool) {
	f, err := h.root.Open(name)
	if err != nil {
		return nil, nil, false
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, false
	}
	return f, info, true
}

func (h *handler) notFound(w http.ResponseWriter, r *http.Request) {
	if h.notFoundPage == "" {
		http.NotFound(w, r)
		return
	}
	h.serveFile(w, r, h.notFoundPage, http.StatusNotFound)
}

// serveFile serves a configured page. 200 responses support conditional and
// range requests; other statuses are sent as-is and never cached.
func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, name string, status int) {
	f, info, ok := h.openRegular(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	hdr := w.Header()
	h.setHeaders(hdr, name)
	if status == http.StatusOK {
		http.ServeContent(w, r, name, info.ModTime(), f)
		return
	}
	hdr.Set("Cache-Control", "no-store")
	if ctype := mime.TypeByExtension(path.Ext(name)); ctype != "" {
		hdr.Set("Content-Type", ctype)
	}
	hdr.Set("Content-Length", fmt.Sprint(info.Size()))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		io.Copy(w, f)
	}
}

// rootFS is an http.FileSystem backed by os.Root, so paths and symlinks
// cannot escape the content directory. It returns *os.File directly, which
// keeps the kernel sendfile fast path in net/http.
type rootFS struct {
	root *os.Root
}

func (f rootFS) Open(name string) (http.File, error) {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if name == "" {
		name = "."
	}
	file, err := f.root.Open(name)
	if err != nil {
		return nil, mapErr(err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, mapErr(err)
	}
	// Directories are only served when they have an index.html (no listings).
	if info.IsDir() {
		idx, err := f.root.Stat(path.Join(name, "index.html"))
		if err != nil || !idx.Mode().IsRegular() {
			file.Close()
			return nil, fs.ErrNotExist
		}
	}
	return file, nil
}

// mapErr hides internal details (e.g. root escape attempts) behind a 404.
func mapErr(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fs.ErrPermission
	}
	return fs.ErrNotExist
}
