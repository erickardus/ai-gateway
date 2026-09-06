package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// dist holds the built single-page application.
//
// The directory is a build output, so the repository carries only a .gitkeep to
// keep the embed directive satisfiable — `all:` matches dotfiles, which is what
// lets `go build` succeed on a clean checkout where `make ui` has not run. A
// binary built that way serves NotBuilt below rather than failing to compile,
// because a missing front-end asset should not stop the gateway from proxying
// inference.
//
//go:embed all:dist
var dist embed.FS

// indexPath is the SPA's entry document, and the marker for whether the front
// end was built at all.
const indexPath = "index.html"

// Assets returns a handler for the built UI, stripped of prefix, and reports
// whether the front end was actually built.
//
// A caller that ignores the second return value still gets a handler: the
// not-built one, which explains what to run. That is the useful failure — an
// operator who enabled the UI and got a blank page needs to be told about
// `make ui`, not given a 404 to guess at.
func Assets(prefix string) (http.Handler, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return NotBuilt(), false
	}
	if _, err := fs.Stat(sub, indexPath); err != nil {
		return NotBuilt(), false
	}
	files := http.FileServerFS(sub)
	return http.StripPrefix(prefix, spa{fsys: sub, files: files}), true
}

// spa serves built assets, falling back to index.html so that a client-side
// route survives a page reload.
//
// The fallback is limited to paths that are not asset requests. Serving
// index.html in place of a missing /assets/main-abc123.js would answer a stale
// or wrong bundle URL with HTML, and the browser's error would be a syntax
// error in a script rather than the 404 that says what actually happened.
type spa struct {
	fsys  fs.FS
	files http.Handler
}

func (s spa) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = indexPath
	}

	if _, err := fs.Stat(s.fsys, name); err == nil {
		// Vite fingerprints every emitted asset, so a URL under assets/ names
		// exactly one immutable byte sequence and can be cached indefinitely.
		// index.html is the opposite: it is the document that points at the
		// current fingerprints, so caching it would pin a browser to the
		// previous deployment's bundle.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-store")
		}
		s.files.ServeHTTP(w, r)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(name, "assets/") {
		http.NotFound(w, r)
		return
	}

	index, err := fs.ReadFile(s.fsys, indexPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(index)
}

// NotBuilt answers every UI request with the command that would fix it.
func NotBuilt() http.Handler {
	const page = `<!doctype html><meta charset="utf-8"><title>Admin UI not built</title>` +
		`<body style="font:14px system-ui;margin:3rem auto;max-width:34rem;line-height:1.6">` +
		`<h1 style="font-size:1.1rem">The admin UI was not built into this binary.</h1>` +
		`<p>The gateway is serving traffic normally; only this page is missing. Build the ` +
		`front end and rebuild the binary:</p>` +
		`<pre style="background:#f4f4f5;padding:.75rem;border-radius:6px">make ui &amp;&amp; make build</pre>` +
		`</body>`
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(page))
	})
}
