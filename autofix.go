package main

// Safe automatic fixes applied to uploaded files before they are checked and
// published. They only touch the temporary copy, never the user's folder,
// and every fix is reported to the user as an "info" note.

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// homeNames are other names people give their home page, in order of
// preference. Web servers on Linux need exactly "index.html".
var homeNames = []string{"index.htm", "default.html", "default.htm", "home.html", "home.htm", "main.html"}

// autoFixIndex makes sure dir has an index.html when the home page has a
// different name. It returns true when it created one.
func (w *Workspace) autoFixIndex(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	var htmls []string
	byLower := map[string]string{}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		l := strings.ToLower(n)
		byLower[l] = n
		if strings.HasSuffix(l, ".html") || strings.HasSuffix(l, ".htm") {
			htmls = append(htmls, n)
		}
	}
	if _, ok := byLower["index.html"]; ok && byLower["index.html"] == "index.html" {
		return false
	}
	pick, why := "", ""
	if n, ok := byLower["index.html"]; ok {
		pick, why = n, "The home page was called "+n+". Web servers need the exact name index.html (small letters), so a copy named index.html was added."
	}
	if pick == "" {
		for _, h := range homeNames {
			if n, ok := byLower[h]; ok {
				pick, why = n, "There was no index.html, so "+n+" was used as the home page (copied to index.html)."
				break
			}
		}
	}
	if pick == "" && len(htmls) == 1 {
		pick, why = htmls[0], "There was no index.html, but only one page ("+htmls[0]+"), so it was used as the home page (copied to index.html)."
	}
	if pick == "" {
		sort.Strings(htmls)
		return false
	}
	if err := copyFileOS(filepath.Join(dir, pick), filepath.Join(dir, "index.html")); err != nil {
		return false
	}
	w.issue("info", "auto-fix", "Fixed automatically: "+why, "")
	return true
}

func copyFileOS(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
