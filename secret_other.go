//go:build !windows

package main

import "strings"

// On non-Windows builds (used for development/testing) the token is stored in
// a 0600 file without extra encryption.
func protectString(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	return "plain:" + s, nil
}

func unprotectString(s string) (string, error) {
	return strings.TrimPrefix(s, "plain:"), nil
}

const secretStorageNote = "stored in a user-only (0600) file; DPAPI encryption is used on Windows"
