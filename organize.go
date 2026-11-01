package main

// v0.4 organisation: groups (folders), coloured tags, pins, archive, notes,
// bulk actions, duplication and the config migration.
//
// Everything in this file only changes Dilreet's own settings on this PC,
// except the "redeploy" bulk action, which the UI only sends after the user
// confirms it.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const currentConfigVersion = 4

type Group struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// palette is the set of colours offered for groups and tags (names, so the
// UI can map them to light and dark theme shades).
var palette = []string{"blue", "green", "orange", "purple", "pink", "teal", "red", "gray"}

func validColor(c string) bool {
	for _, p := range palette {
		if p == c {
			return true
		}
	}
	return false
}

// migrateConfig upgrades settings written by older versions. It never removes
// data; it only fills new fields and cleans references that point nowhere.
func migrateConfig(c *Config) (changed bool) {
	if c.ConfigVersion < currentConfigVersion {
		if c.ConfigVersion == 0 && c.MigratedFrom == "" && len(c.Sites) > 0 {
			c.MigratedFrom = "0.3"
		}
		if c.Backup == nil && len(c.Sites) > 0 {
			// Existing users get weekly local backups of their current files;
			// new users choose during setup.
			c.Backup = &BackupSettings{Every: "weekly", Keep: 8}
		}
		c.ConfigVersion = currentConfigVersion
		changed = true
	}
	if normalizeOrg(c) {
		changed = true
	}
	return changed
}

// normalizeOrg removes duplicate tags, unknown groups and bad colours.
func normalizeOrg(c *Config) (changed bool) {
	ids := map[string]bool{}
	keep := c.Groups[:0]
	for _, g := range c.Groups {
		if g == nil || g.ID == "" || ids[g.ID] {
			changed = true
			continue
		}
		if !validColor(g.Color) {
			g.Color, changed = "blue", true
		}
		ids[g.ID] = true
		keep = append(keep, g)
	}
	c.Groups = keep
	for _, s := range c.Sites {
		if s.Group != "" && !ids[s.Group] {
			s.Group, changed = "", true
		}
		t := cleanTags(s.Tags)
		if strings.Join(t, "\x00") != strings.Join(s.Tags, "\x00") {
			s.Tags, changed = t, true
		}
	}
	for k, v := range c.TagColors {
		if !validColor(v) {
			delete(c.TagColors, k)
			changed = true
		}
	}
	return changed
}

var tagBad = regexp.MustCompile(`[^\p{L}\p{M}\p{N} _.+#&-]`)

func cleanTag(t string) string {
	t = strings.TrimSpace(tagBad.ReplaceAllString(t, ""))
	if len([]rune(t)) > 24 {
		t = string([]rune(t)[:24])
	}
	return strings.ToLower(t)
}

func cleanTags(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range in {
		t = cleanTag(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

func groupID(name string, c *Config) string {
	base := slugify(name)
	if base == "" {
		base = "group"
	}
	id := base
	for i := 2; ; i++ {
		dup := false
		for _, g := range c.Groups {
			if g.ID == id {
				dup = true
			}
		}
		if !dup {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
}

func allTags(c *Config) []map[string]any {
	n := map[string]int{}
	for _, s := range c.Sites {
		for _, t := range s.Tags {
			n[t]++
		}
	}
	for t := range c.TagColors {
		if _, ok := n[t]; !ok {
			n[t] = 0
		}
	}
	out := []map[string]any{}
	for t, k := range n {
		col := c.TagColors[t]
		if col == "" {
			col = palette[int(hashStr(t))%len(palette)]
		}
		out = append(out, map[string]any{"name": t, "color": col, "count": k})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return out
}

func hashStr(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func orgPayload(c *Config) map[string]any {
	counts := map[string]int{}
	for _, s := range c.Sites {
		if !s.Archived {
			counts[s.Group]++
		}
	}
	groups := []map[string]any{}
	for _, g := range c.Groups {
		groups = append(groups, map[string]any{"id": g.ID, "name": g.Name, "color": g.Color, "count": counts[g.ID]})
	}
	return map[string]any{"groups": groups, "tags": allTags(c), "palette": palette, "ungrouped": counts[""]}
}

// SiteMeta is a partial update; nil fields are left alone.
type SiteMeta struct {
	Group    *string   `json:"group"`
	Tags     *[]string `json:"tags"`
	AddTag   string    `json:"add_tag"`
	DelTag   string    `json:"remove_tag"`
	Pinned   *bool     `json:"pinned"`
	Archived *bool     `json:"archived"`
	Notes    *string   `json:"notes"`
}

func applyMeta(c *Config, s *Site, m SiteMeta) error {
	if m.Group != nil {
		g := *m.Group
		if g != "" {
			ok := false
			for _, x := range c.Groups {
				if x.ID == g {
					ok = true
				}
			}
			if !ok {
				return errors.New("that group does not exist any more")
			}
		}
		s.Group = g
	}
	if m.Tags != nil {
		s.Tags = cleanTags(*m.Tags)
	}
	if m.AddTag != "" {
		s.Tags = cleanTags(append(s.Tags, m.AddTag))
	}
	if m.DelTag != "" {
		t := cleanTag(m.DelTag)
		out := []string{}
		for _, x := range s.Tags {
			if x != t {
				out = append(out, x)
			}
		}
		s.Tags = out
	}
	if m.Pinned != nil {
		s.Pinned = *m.Pinned
	}
	if m.Archived != nil {
		s.Archived = *m.Archived
		if s.Archived {
			s.Pinned = false
		}
	}
	if m.Notes != nil {
		n := *m.Notes
		if len(n) > 4000 {
			n = n[:4000]
		}
		s.Notes = n
	}
	return nil
}

// stageFolder stages a local folder as an import so it can go through the
// normal review -> publish flow (used by templates and duplication).
func stageFolder(dir, name, kind string) (map[string]any, error) {
	entries, err := dirEntries(dir)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("there are no files to copy")
	}
	ws, err := newWorkspace(entries, dir)
	if err != nil {
		return nil, err
	}
	st := stage(&FetchResult{WS: ws, Ref: &RepoRef{Kind: "upload", Site: name, Display: kind}}, "")
	out := stagedSummary(st)
	out["suggested_name"] = name
	return out, nil
}

func registerOrgRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/org", func(w http.ResponseWriter, r *http.Request) {
		c := snapshotConfig()
		writeJSON(w, 200, orgPayload(&c))
	})
	mux.HandleFunc("POST /api/org/groups", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Color  string `json:"color"`
			Delete bool   `json:"delete"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		var id string
		err := withConfig(func(c *Config) error {
			if req.Delete {
				out := c.Groups[:0]
				for _, g := range c.Groups {
					if g.ID != req.ID {
						out = append(out, g)
					}
				}
				c.Groups = out
				for _, s := range c.Sites {
					if s.Group == req.ID {
						s.Group = ""
					}
				}
				return nil
			}
			if req.Name == "" || len([]rune(req.Name)) > 40 {
				return errors.New("give the group a name (up to 40 letters)")
			}
			if !validColor(req.Color) {
				req.Color = palette[len(c.Groups)%len(palette)]
			}
			for _, g := range c.Groups {
				if g.ID == req.ID {
					g.Name, g.Color, id = req.Name, req.Color, g.ID
					return nil
				}
				if req.ID == "" && strings.EqualFold(g.Name, req.Name) {
					return errors.New("a group with this name already exists")
				}
			}
			if len(c.Groups) >= 50 {
				return errors.New("you can have up to 50 groups")
			}
			id = groupID(req.Name, c)
			c.Groups = append(c.Groups, &Group{ID: id, Name: req.Name, Color: req.Color})
			return nil
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		c := snapshotConfig()
		out := orgPayload(&c)
		out["id"] = id
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /api/org/tags", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name   string `json:"name"`
			Color  string `json:"color"`
			Rename string `json:"rename"`
			Delete bool   `json:"delete"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		t := cleanTag(req.Name)
		if t == "" {
			writeErr(w, errors.New("tag name is empty"))
			return
		}
		err := withConfig(func(c *Config) error {
			if c.TagColors == nil {
				c.TagColors = map[string]string{}
			}
			if req.Delete {
				delete(c.TagColors, t)
				for _, s := range c.Sites {
					_ = applyMeta(c, s, SiteMeta{DelTag: t})
				}
				return nil
			}
			if nt := cleanTag(req.Rename); nt != "" && nt != t {
				for _, s := range c.Sites {
					for i, x := range s.Tags {
						if x == t {
							s.Tags[i] = nt
						}
					}
					s.Tags = cleanTags(s.Tags)
				}
				if col, ok := c.TagColors[t]; ok {
					c.TagColors[nt] = col
					delete(c.TagColors, t)
				}
				t = nt
			}
			if validColor(req.Color) {
				c.TagColors[t] = req.Color
			}
			return nil
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		c := snapshotConfig()
		writeJSON(w, 200, orgPayload(&c))
	})
	mux.HandleFunc("POST /api/sites/{slug}/meta", func(w http.ResponseWriter, r *http.Request) {
		var m SiteMeta
		if err := readJSON(r, &m); err != nil {
			writeErr(w, err)
			return
		}
		slug := r.PathValue("slug")
		var out Site
		err := withConfig(func(c *Config) error {
			s := findSite(c, slug)
			if s == nil {
				return errors.New("project not found")
			}
			if err := applyMeta(c, s, m); err != nil {
				return err
			}
			out = *s
			return nil
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "site": out})
	})
	mux.HandleFunc("POST /api/bulk", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action string   `json:"action"`
			Slugs  []string `json:"slugs"`
			Value  string   `json:"value"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if len(req.Slugs) == 0 {
			writeErr(w, errors.New("select at least one project"))
			return
		}
		switch req.Action {
		case "redeploy":
			slugs := append([]string(nil), req.Slugs...)
			j := newJob("bulk-redeploy", func(j *Job) error { return redeployMany(j, slugs) })
			writeJSON(w, 200, map[string]any{"job": j.ID})
			return
		case "health":
			res := runHealthFor(req.Slugs)
			writeJSON(w, 200, map[string]any{"results": res})
			return
		}
		t, f := true, false
		var m SiteMeta
		switch req.Action {
		case "pin":
			m.Pinned = &t
		case "unpin":
			m.Pinned = &f
		case "archive":
			m.Archived = &t
		case "unarchive":
			m.Archived = &f
		case "group":
			v := req.Value
			m.Group = &v
		case "tag":
			m.AddTag = req.Value
		case "untag":
			m.DelTag = req.Value
		default:
			writeErr(w, errors.New("unknown action"))
			return
		}
		n := 0
		err := withConfig(func(c *Config) error {
			for _, slug := range req.Slugs {
				if s := findSite(c, slug); s != nil {
					if err := applyMeta(c, s, m); err != nil {
						return err
					}
					n++
				}
			}
			return nil
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "changed": n})
	})
	mux.HandleFunc("POST /api/sites/{slug}/duplicate", func(w http.ResponseWriter, r *http.Request) {
		dir, s, err := projectDir(r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return
		}
		cur := filepath.Join(dir, "current")
		if fi, err := os.Stat(filepath.Join(cur, "index.html")); err != nil || fi.IsDir() {
			writeErr(w, errors.New("this project has no files on this PC yet. Deploy it once, then copy it"))
			return
		}
		out, err := stageFolder(cur, s.Name+" copy", "copy of "+s.Name)
		if err != nil {
			writeErr(w, err)
			return
		}
		out["copy_of"] = s.Slug
		out["group"] = s.Group
		out["tags"] = s.Tags
		writeJSON(w, 200, out)
	})
}

// redeployMany publishes each project's current local files again, one after
// another. Used by the bulk "Redeploy" action (the UI asks first).
func redeployMany(j *Job, slugs []string) error {
	var failed []string
	j.SetSteps(slugs...)
	for i, slug := range slugs {
		j.Step(i)
		dir, _, err := projectDir(slug)
		if err == nil {
			var entries []srcEntry
			if entries, err = dirEntries(filepath.Join(dir, "current")); err == nil {
				if len(entries) == 0 {
					err = errors.New("no files on this PC")
				} else {
					var ws *Workspace
					if ws, err = newWorkspace(entries, filepath.Join(dir, "current")); err == nil {
						err = deployWorkspace(j, slug, ws)
						ws.Close()
					}
				}
			}
		}
		if err != nil {
			j.Logf("%s: %v", slug, err)
			failed = append(failed, slug)
			continue
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d did not deploy: %s", len(failed), len(slugs), strings.Join(failed, ", "))
	}
	return nil
}
