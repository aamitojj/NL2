//go:build windows

package main

import (
	"encoding/base64"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// protectString encrypts s with DPAPI for the current Windows user.
func protectString(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	b := []byte(s)
	in := windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return "", err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	enc := unsafe.Slice(out.Data, out.Size)
	return "dpapi:" + base64.StdEncoding.EncodeToString(enc), nil
}

func unprotectString(s string) (string, error) {
	if !strings.HasPrefix(s, "dpapi:") {
		return strings.TrimPrefix(s, "plain:"), nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "dpapi:"))
	if err != nil || len(raw) == 0 {
		return "", err
	}
	in := windows.DataBlob{Size: uint32(len(raw)), Data: &raw[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return "", err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return string(unsafe.Slice(out.Data, out.Size)), nil
}

const secretStorageNote = "encrypted with Windows DPAPI (only your Windows user can decrypt it)"
