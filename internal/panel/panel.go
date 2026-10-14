// Package panel serves the embedded web console.
//
// The console is a static single-page app built from web/ and embedded at
// compile time, so the gateway ships as a single binary.
package panel

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed dist
var distFS embed.FS

// Handler serves the console rooted at /panel/.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return http.NotFoundHandler()
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		p := strings.TrimPrefix(r.URL.Path, "/panel")
		if p == "" {
			p = "/"
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = p
		fileServer.ServeHTTP(w, r2)
	})
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
}
