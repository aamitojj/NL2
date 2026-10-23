//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// hideConsole stops child tools (git, npm) from flashing a console window.
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// documentsDir is the user's Documents folder (follows OneDrive redirection).
func documentsDir() string {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_Documents, 0); err == nil && p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Documents")
}

// revealInExplorer opens Explorer at dir, or with file selected.
func revealInExplorer(path string, isFile bool) error {
	if isFile {
		return exec.Command("explorer.exe", "/select,", path).Start()
	}
	return exec.Command("explorer.exe", path).Start()
}

// openInEditor opens a file for editing with the program Windows associates
// with "edit" (falling back to Notepad), so it never runs the file.
func openInEditor(path string) error {
	verb, _ := windows.UTF16PtrFromString("edit")
	file, _ := windows.UTF16PtrFromString(path)
	if !isRiskyToOpen(path) {
		if err := windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL); err == nil {
			return nil
		}
	}
	return exec.Command("notepad.exe", path).Start()
}
