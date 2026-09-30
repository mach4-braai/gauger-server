// Package ui serves the web UI on the tailnet-only listener.
package ui

import (
	"bytes"
	"embed"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

type UI struct {
	Store  *store.Store
	GitHub *github.Client
	Creds  github.CredentialSource
	// DNSName is the server's MagicDNS name, used in the App manifest.
	DNSName string
	// GitHubURL is the web origin, https://github.com unless testing.
	GitHubURL string

	pages map[string]*template.Template
}

func (u *UI) Handler() http.Handler {
	u.pages = map[string]*template.Template{}
	for _, name := range []string{"setup", "setup_redirect"} {
		u.pages[name] = template.Must(template.New("layout.html").Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html"))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /setup", u.setup)
	mux.HandleFunc("POST /setup/manifest", u.setupManifest)
	mux.HandleFunc("GET /setup/callback", u.setupCallback)
	return mux
}

var funcs = template.FuncMap{}

func (u *UI) render(w http.ResponseWriter, page string, data any) {
	var buf bytes.Buffer
	if err := u.pages[page].Execute(&buf, data); err != nil {
		slog.Error("render", "page", page, "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}
