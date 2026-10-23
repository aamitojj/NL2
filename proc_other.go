//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func hideConsole(cmd *exec.Cmd) {}

func documentsDir() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Documents")
}

func revealInExplorer(path string, isFile bool) error {
	if isFile {
		path = filepath.Dir(path)
	}
	if runtime.GOOS == "darwin" {
		return exec.Command("open", path).Start()
	}
	return exec.Command("xdg-open", path).Start()
}

func openInEditor(path string) error {
	if isRiskyToOpen(path) {
		return revealInExplorer(path, true)
	}
	if runtime.GOOS == "darwin" {
		return exec.Command("open", "-t", path).Start()
	}
	return exec.Command("xdg-open", path).Start()
}
