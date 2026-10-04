// Package static serves the dashboard's embedded assets. Each is served
// under a name holding a hash of its content, so it can be cached forever.
package static

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

//go:embed datastar.js stats.css
var files embed.FS

type asset struct {
	contentType string
	body, gz    []byte
}

var (
	// byPath maps a hashed path, /static/datastar.<hash>.js, to its asset.
	byPath = map[string]asset{}
	// paths maps a file name to its hashed path.
	paths = map[string]string{}
)

func init() {
	names, err := fs.Glob(files, "*")
	if err != nil {
		panic(err)
	}
	for _, name := range names {
		body, err := files.ReadFile(name)
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(body)
		ext := path.Ext(name)
		p := "/static/" + strings.TrimSuffix(name, ext) + "." + hex.EncodeToString(sum[:6]) + ext
		var gz bytes.Buffer
		w, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
		w.Write(body)
		w.Close()
		ct := mime.TypeByExtension(ext)
		if ext == ".js" {
			ct = "text/javascript; charset=utf-8"
		}
		byPath[p] = asset{contentType: ct, body: body, gz: gz.Bytes()}
		paths[name] = p
	}
}

// Path returns the hashed URL path of an embedded file, such as
// /static/datastar.727844adfc82.js for datastar.js. It panics for a file
// that is not embedded.
func Path(name string) string {
	p, ok := paths[name]
	if !ok {
		panic("static: no embedded file " + name)
	}
	return p
}

// Handler serves the hashed paths, gzipped when the client accepts it.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := byPath[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", a.contentType)
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("Vary", "Accept-Encoding")
		body := a.body
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.Set("Content-Encoding", "gzip")
			body = a.gz
		}
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	})
}
