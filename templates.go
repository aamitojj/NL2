package main

// Starter templates: small, ready-made static sites (written for Dilreet,
// no third-party code). Creating from a template copies the files to a
// temporary folder, fills in the site name and sends them through the normal
// review -> publish flow.

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

//go:embed starters
var startersFS embed.FS

type Starter struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Desc  string   `json:"desc"`
	Color string   `json:"color"`
	Files []string `json:"files"`
}

var starterOrder = []string{"business", "portfolio", "links", "restaurant", "event", "coming-soon", "blank"}

func listStarters() []Starter {
	ents, _ := fs.ReadDir(startersFS, "starters")
	var out []Starter
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		b, err := startersFS.ReadFile("starters/" + e.Name() + "/starter.json")
		if err != nil {
			continue
		}
		var s Starter
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		s.ID = e.Name()
		files, _ := fs.ReadDir(startersFS, "starters/"+e.Name())
		for _, f := range files {
			if f.Name() != "starter.json" {
				s.Files = append(s.Files, f.Name())
			}
		}
		out = append(out, s)
	}
	pos := func(id string) int {
		for i, x := range starterOrder {
			if x == id {
				return i
			}
		}
		return len(starterOrder)
	}
	sort.SliceStable(out, func(i, j int) bool { return pos(out[i].ID) < pos(out[j].ID) })
	return out
}

func fillStarter(b []byte, name string) []byte {
	ini := "S"
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			ini = strings.ToUpper(string(r))
			break
		}
	}
	r := strings.NewReplacer("{{SITE_NAME}}", htmlEscape(name), "{{YEAR}}", strconv.Itoa(time.Now().Year()), "{{INITIAL}}", htmlEscape(ini))
	return []byte(r.Replace(string(b)))
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}

// writeStarter copies a starter into dir with the name filled in.
func writeStarter(id, name, dir string) error {
	root := "starters/" + id
	if _, err := fs.Stat(startersFS, root+"/starter.json"); err != nil || strings.ContainsAny(id, "./\\") {
		return errors.New("unknown template")
	}
	return fs.WalkDir(startersFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Base(p) == "starter.json" {
			return err
		}
		b, err := startersFS.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, root+"/")
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".css") {
			b = fillStarter(b, name)
		}
		return os.WriteFile(dst, b, 0o644)
	})
}

func registerTemplateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/templates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"templates": listStarters()})
	})
	// Preview: serves a template's files with a sample name (for thumbnails).
	mux.HandleFunc("GET /api/templates/{id}/preview/{file...}", func(w http.ResponseWriter, r *http.Request) {
		id, f := r.PathValue("id"), r.PathValue("file")
		if f == "" {
			f = "index.html"
		}
		if strings.Contains(id, "..") || strings.Contains(f, "..") || f == "starter.json" {
			http.NotFound(w, r)
			return
		}
		b, err := startersFS.ReadFile("starters/" + id + "/" + f)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "Sharma Store"
		}
		w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(f)))
		w.Header().Del("X-Frame-Options") // shown as a thumbnail inside Dilreet only
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self' data:; frame-ancestors 'self'")
		out := fillStarter(b, name)
		// The thumbnail iframe is sandboxed (no cookies), so put the CSS inline.
		if css, err := startersFS.ReadFile("starters/" + id + "/style.css"); err == nil && strings.HasSuffix(f, ".html") {
			out = bytes.Replace(out, []byte(`<link rel="stylesheet" href="style.css">`), append(append([]byte("<style>"), css...), "</style>"...), 1)
		}
		_, _ = w.Write(out)
	})
	mux.HandleFunc("POST /api/templates/{id}/use", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		_ = readJSON(r, &req)
		id := r.PathValue("id")
		name := strings.TrimSpace(req.Name)
		if name == "" {
			for _, s := range listStarters() {
				if s.ID == id {
					name = "My " + strings.ToLower(s.Name)
				}
			}
		}
		if len([]rune(name)) > 60 {
			writeErr(w, errors.New("the name is too long (60 letters at most)"))
			return
		}
		dir, err := os.MkdirTemp("", "dilreet-starter-")
		if err != nil {
			writeErr(w, err)
			return
		}
		defer os.RemoveAll(dir)
		if err := writeStarter(id, name, dir); err != nil {
			writeErr(w, err)
			return
		}
		out, err := stageFolder(dir, name, "template "+id)
		if err != nil {
			writeErr(w, err)
			return
		}
		out["template"] = id
		writeJSON(w, 200, out)
	})
}
