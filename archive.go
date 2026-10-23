package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// srcEntry is one file of the uploaded site.
type srcEntry struct {
	Path string // slash-separated, relative
	Size int64
	Open func() (io.ReadCloser, error)
}

const (
	maxSiteFiles = 100000
	maxSiteBytes = 2 << 30 // 2 GiB uncompressed
)

var skipNames = map[string]bool{".DS_Store": true, "Thumbs.db": true, "desktop.ini": true}

// cleanRel normalises an archive path and rejects anything that could escape
// the site directory ("zip slip"). Returns "" for entries to skip.
func cleanRel(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") || (len(p) > 1 && p[1] == ':') {
		return "", fmt.Errorf("absolute path not allowed in upload: %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path with '..' not allowed in upload: %q", p)
		}
	}
	c := path.Clean(p)
	if c == "." || c == "" {
		return "", nil
	}
	if strings.HasPrefix(c, "__MACOSX/") || c == "__MACOSX" || skipNames[path.Base(c)] {
		return "", nil
	}
	return c, nil
}

func zipEntries(zipPath string) ([]srcEntry, func(), error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, nil, fmt.Errorf("not a valid .zip file: %w", err)
	}
	var out []srcEntry
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !f.Mode().IsRegular() {
			continue
		}
		rel, err := cleanRel(f.Name)
		if err != nil {
			zr.Close()
			return nil, nil, err
		}
		if rel == "" {
			continue
		}
		f := f
		out = append(out, srcEntry{Path: rel, Size: int64(f.UncompressedSize64), Open: func() (io.ReadCloser, error) { return f.Open() }})
	}
	return out, func() { zr.Close() }, nil
}

func dirEntries(root string) ([]srcEntry, error) {
	var out []srcEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		relOS, _ := filepath.Rel(root, p)
		rel, err := cleanRel(filepath.ToSlash(relOS))
		if err != nil || rel == "" {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		pp := p
		out = append(out, srcEntry{Path: rel, Size: info.Size(), Open: func() (io.ReadCloser, error) { return os.Open(pp) }})
		return nil
	})
	return out, err
}

// stripCommonRoot removes wrapper folders (e.g. "my-site/" or "dist/") while
// there is no index.html at the top level and every file shares one folder.
func stripCommonRoot(entries []srcEntry) []srcEntry {
	for {
		if len(entries) == 0 {
			return entries
		}
		hasIndex := false
		first := ""
		same := true
		for _, e := range entries {
			if e.Path == "index.html" {
				hasIndex = true
			}
			i := strings.IndexByte(e.Path, '/')
			if i < 0 {
				same = false
				continue
			}
			top := e.Path[:i]
			if first == "" {
				first = top
			} else if top != first {
				same = false
			}
		}
		if hasIndex || !same || first == "" {
			return entries
		}
		for i := range entries {
			entries[i].Path = entries[i].Path[len(first)+1:]
		}
	}
}

// buildSiteTarball writes entries to a .tar.gz temp file and returns its path.
func buildSiteTarball(entries []srcEntry) (string, int, int64, error) {
	entries = stripCommonRoot(entries)
	if len(entries) == 0 {
		return "", 0, 0, errors.New("the upload contains no files")
	}
	if len(entries) > maxSiteFiles {
		return "", 0, 0, fmt.Errorf("too many files (%d, max %d)", len(entries), maxSiteFiles)
	}
	var total int64
	hasIndex := false
	for _, e := range entries {
		total += e.Size
		if e.Path == "index.html" {
			hasIndex = true
		}
	}
	if total > maxSiteBytes {
		return "", 0, 0, fmt.Errorf("site is too large (%d MB, max %d MB)", total>>20, maxSiteBytes>>20)
	}
	if !hasIndex {
		return "", 0, 0, errors.New("no index.html found at the top level of the upload. Zip the folder that contains index.html (for built sites usually 'dist', 'build' or 'public')")
	}
	f, err := os.CreateTemp("", "dilreet-site-*.tar.gz")
	if err != nil {
		return "", 0, 0, err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	now := time.Now()
	fail := func(err error) (string, int, int64, error) {
		tw.Close()
		gz.Close()
		f.Close()
		os.Remove(f.Name())
		return "", 0, 0, err
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Path] {
			continue
		}
		seen[e.Path] = true
		rc, err := e.Open()
		if err != nil {
			return fail(err)
		}
		hdr := &tar.Header{Name: e.Path, Mode: 0o644, Size: e.Size, ModTime: now, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			rc.Close()
			return fail(err)
		}
		n, err := io.Copy(tw, io.LimitReader(rc, e.Size))
		rc.Close()
		if err != nil {
			return fail(err)
		}
		if n != e.Size {
			return fail(fmt.Errorf("file %s changed size while reading", e.Path))
		}
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	return f.Name(), len(seen), total, nil
}
