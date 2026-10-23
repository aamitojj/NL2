package main

// Optional local build (npm) for imported or dropped source projects. Only
// runs after the user clicks "Build it for me" (it executes the project's own
// npm scripts on this PC, which the consent text says).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// buildPlan says whether a workspace needs a build and how.
type buildPlan struct {
	Needed   bool   `json:"needed"`
	Command  string `json:"command,omitempty"`
	Install  string `json:"install,omitempty"`
	NodeOK   bool   `json:"node_ok"`
	NodeHint string `json:"node_hint,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func planBuild(ws *Workspace) buildPlan {
	p := buildPlan{}
	pr := ws.Project
	if !pr.PackageJSON || !pr.BuildScript {
		return p
	}
	// A package.json with a build script whose publish folder still holds
	// sources (or no index.html at all) needs building.
	hasIndex := ws.Files != nil && ws.Files.IsFile("index.html")
	if !pr.SourceProject && hasIndex {
		return p
	}
	p.Needed = true
	p.Command = "npm run build"
	if pr.TomlCommand != "" && strings.HasPrefix(strings.TrimSpace(pr.TomlCommand), "npm ") {
		p.Command = strings.TrimSpace(pr.TomlCommand)
	}
	p.Install = "npm install"
	if isFileOS(filepath.Join(ws.Root, "package-lock.json")) {
		p.Install = "npm ci"
	}
	p.Reason = pr.Framework + " project: the files must be built before they can be published."
	if _, err := exec.LookPath("npm"); err == nil {
		p.NodeOK = true
	} else {
		p.NodeHint = "Node.js is not installed on this PC. Install the LTS version from https://nodejs.org, restart Dilreet, then click Build again. Or build it yourself and drop the output folder (usually dist or build)."
	}
	return p
}

var allowedBuildCmd = map[string]bool{"npm run build": true, "npm ci": true, "npm install": true}

// runBuild runs install + build inside the workspace, then re-detects the
// publish folder. Output streams into the job log.
func runBuild(ctx context.Context, j *Job, ws *Workspace) error {
	p := planBuild(ws)
	if !p.Needed {
		j.Logf("No build needed.")
		return nil
	}
	if !p.NodeOK {
		return errors.New(p.NodeHint)
	}
	npm, _ := exec.LookPath("npm")
	run := func(cmdline string) error {
		args := strings.Fields(cmdline)[1:]
		if !allowedBuildCmd[cmdline] && !(len(args) >= 2 && args[0] == "run") {
			return fmt.Errorf("refusing unusual build command %q; build it yourself and drop the output folder", cmdline)
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(cctx, npm, args...)
		cmd.Dir = ws.Root
		cmd.Env = append(os.Environ(), "CI=1", "NODE_ENV=production", "npm_config_fund=false", "npm_config_audit=false", "npm_config_update_notifier=false")
		hideConsole(cmd)
		cmd.Stdout, cmd.Stderr = j, j
		j.Logf("$ %s", cmdline)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%q failed: %v (see the log above; click 'Ask assistant about this' for help)", cmdline, err)
		}
		return nil
	}
	j.SetProgress(-1, "Installing dependencies")
	if err := run(p.Install); err != nil {
		if p.Install == "npm ci" { // lockfile out of sync: the classic auto-fix
			j.Logf("npm ci failed; retrying with npm install (lockfile may be out of date)...")
			if err2 := run("npm install"); err2 != nil {
				return err2
			}
		} else {
			return err
		}
	}
	j.SetProgress(-1, "Building")
	if err := run(p.Command); err != nil {
		return err
	}
	// node_modules is never published; drop it before re-indexing.
	_ = os.RemoveAll(filepath.Join(ws.Root, "node_modules"))
	ws.Issues, ws.Bytes = nil, 0
	ws.detectPublish()
	if err := ws.index(); err != nil {
		return err
	}
	ws.detectProject()
	ws.Project.SourceProject = false
	if !ws.Files.IsFile("index.html") {
		return errors.New("the build finished but produced no index.html (check the project's build output folder)")
	}
	j.Logf("Build finished. Publishing folder: %s (%d files).", firstNonEmpty(ws.PublishRel, "top level"), ws.FileCount)
	return nil
}
