package main

// Scheduled backups: a zip of every project's current files (and optionally
// every saved deploy) plus a plain list of projects, written to a folder the
// user picks (for example a OneDrive / Google Drive folder or a USB disk).
// Secrets (tokens, passwords) are never written to a backup.

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type BackupSettings struct {
	Every          string `json:"every"` // off | daily | weekly
	Dir            string `json:"dir,omitempty"`
	Keep           int    `json:"keep,omitempty"`
	IncludeDeploys bool   `json:"include_deploys,omitempty"`
	Last           string `json:"last,omitempty"`
	LastFile       string `json:"last_file,omitempty"`
	LastSize       int64  `json:"last_size,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

func backupSettings(c *Config) BackupSettings {
	b := BackupSettings{Every: "off"}
	if c.Backup != nil {
		b = *c.Backup
	}
	if b.Every == "" {
		b.Every = "off"
	}
	if b.Keep <= 0 {
		b.Keep = 8
	}
	if b.Dir == "" {
		b.Dir = filepath.Join(documentsDir(), "Dilreet", "Backups")
	}
	return b
}

func backupDue(b BackupSettings, now time.Time) bool {
	var every time.Duration
	switch b.Every {
	case "daily":
		every = 24 * time.Hour
	case "weekly":
		every = 7 * 24 * time.Hour
	default:
		return false
	}
	t, err := time.Parse(time.RFC3339, b.Last)
	return err != nil || now.Sub(t) >= every
}

// backupNow writes one backup zip and prunes old ones.
func backupNow(c *Config, now time.Time) (string, int64, error) {
	b := backupSettings(c)
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("cannot create the backup folder %s: %v", b.Dir, err)
	}
	name := "dilreet-backup-" + now.Format("2006-01-02_1504") + ".zip"
	dst := filepath.Join(b.Dir, name)
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", 0, err
	}
	zw := zip.NewWriter(f)
	root := projectsRoot(c)
	type meta struct {
		Name, Slug, Domain, Group, Folder string
		Tags                              []string
		Pinned, Archived                  bool
	}
	var list []meta
	for _, s := range c.Sites {
		list = append(list, meta{s.Name, s.Slug, s.Domain, s.Group, s.Folder, s.Tags, s.Pinned, s.Archived})
		if s.Folder == "" {
			continue
		}
		pd := filepath.Join(root, s.Folder)
		add := func(rel string) error {
			p := filepath.Join(pd, rel)
			return filepath.WalkDir(p, func(fp string, d fs.DirEntry, err error) error {
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return nil
					}
					return err
				}
				if !d.Type().IsRegular() {
					return nil
				}
				r, _ := filepath.Rel(root, fp)
				return zipAdd(zw, filepath.ToSlash(filepath.Join("projects", r)), fp)
			})
		}
		if err := add("current"); err != nil {
			zw.Close()
			f.Close()
			os.Remove(tmp)
			return "", 0, err
		}
		if b.IncludeDeploys {
			err = add("deploys")
		} else {
			err = add(filepath.Join("deploys", "index.json"))
		}
		if err != nil {
			zw.Close()
			f.Close()
			os.Remove(tmp)
			return "", 0, err
		}
	}
	js, _ := json.MarshalIndent(map[string]any{"app": "Dilreet " + Version, "time": now.UTC().Format(time.RFC3339), "server": c.ServerIP, "groups": c.Groups, "projects": list}, "", "  ")
	if w, err := zw.Create("dilreet-projects.json"); err == nil {
		_, _ = w.Write(js)
	}
	if w, err := zw.Create("README.txt"); err == nil {
		_, _ = io.WriteString(w, "Dilreet backup\r\n\r\nprojects\\<name>\\current  = the live files of each site\r\nprojects\\<name>\\deploys  = deploy history\r\n\r\nTo restore a site: open Dilreet > Add new project > Drag and drop, and drop the \"current\" folder.\r\nPasswords and tokens are NOT in this file.\r\n")
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", 0, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", 0, err
	}
	st, _ := os.Stat(dst)
	pruneBackups(b.Dir, b.Keep)
	return dst, st.Size(), nil
}

func zipAdd(zw *zip.Writer, name, path string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, src)
	return err
}

func pruneBackups(dir string, keep int) {
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "dilreet-backup-") && strings.HasSuffix(e.Name(), ".zip") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keep {
		_ = os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

func listBackups(dir string) []map[string]any {
	ents, _ := os.ReadDir(dir)
	out := []map[string]any{}
	for i := len(ents) - 1; i >= 0; i-- {
		e := ents[i]
		if e.IsDir() || !strings.HasPrefix(e.Name(), "dilreet-backup-") || !strings.HasSuffix(e.Name(), ".zip") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, map[string]any{"name": e.Name(), "size": fi.Size(), "time": fi.ModTime().UTC().Format(time.RFC3339)})
	}
	return out
}

func runBackup(now time.Time) (string, error) {
	c := snapshotConfig()
	p, size, err := backupNow(&c, now)
	_ = withConfig(func(c *Config) error {
		b := backupSettings(c)
		if err != nil {
			b.LastError = err.Error()
		} else {
			b.Last, b.LastFile, b.LastSize, b.LastError = now.UTC().Format(time.RFC3339), p, size, ""
		}
		c.Backup = &b
		return nil
	})
	return p, err
}

func backupTick(now time.Time) {
	c := snapshotConfig()
	if len(c.Sites) == 0 || !backupDue(backupSettings(&c), now) {
		return
	}
	if _, err := runBackup(now); err != nil {
		notify("warn", "", "The scheduled backup did not work: "+err.Error())
	}
}

func registerBackupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/backup", func(w http.ResponseWriter, r *http.Request) {
		c := snapshotConfig()
		b := backupSettings(&c)
		writeJSON(w, 200, map[string]any{"settings": b, "backups": listBackups(b.Dir), "ssl_warn_days": sslWarnDays(&c)})
	})
	mux.HandleFunc("POST /api/backup/settings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Every          string `json:"every"`
			Dir            string `json:"dir"`
			Keep           int    `json:"keep"`
			IncludeDeploys bool   `json:"include_deploys"`
			SSLWarnDays    int    `json:"ssl_warn_days"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if req.Every != "off" && req.Every != "daily" && req.Every != "weekly" {
			writeErr(w, errors.New("choose off, daily or weekly"))
			return
		}
		if req.Dir != "" && !filepath.IsAbs(req.Dir) {
			writeErr(w, errors.New("the backup folder must be a full path, like D:\\Backups"))
			return
		}
		err := withConfig(func(c *Config) error {
			b := backupSettings(c)
			b.Every, b.IncludeDeploys = req.Every, req.IncludeDeploys
			if req.Keep > 0 && req.Keep <= 100 {
				b.Keep = req.Keep
			}
			if req.Dir != "" {
				b.Dir = req.Dir
			}
			c.Backup = &b
			if req.SSLWarnDays > 0 && req.SSLWarnDays <= 60 {
				c.SSLWarnDays = req.SSLWarnDays
			}
			return nil
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/backup/run", func(w http.ResponseWriter, r *http.Request) {
		j := newJob("backup", func(j *Job) error {
			j.Logf("Making a backup of all project files ...")
			p, err := runBackup(time.Now())
			if err != nil {
				return err
			}
			j.Logf("Saved %s", p)
			j.SetResult(map[string]any{"file": p})
			return nil
		})
		writeJSON(w, 200, map[string]any{"job": j.ID})
	})
}
