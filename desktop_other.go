//go:build !windows

package main

import (
	"errors"
	"time"
)

// Non-Windows builds (development and tests) always run in browser mode.

func isNativePlatform() bool                      { return false }
func hasConsole() bool                            { return true }
func platformInit()                               {}
func appDataLocal() string                        { return "" }
func acquireInstanceLock(wait time.Duration) bool { return true }
func allowForeground(pid int)                     {}
func showInfo(text string)                        { println(text) }
func webView2Status() (string, error)             { return "", errors.New("WebView2 is Windows-only") }
func askNoWebView2(detail string) string          { return "browser" }
func runNativeWindow(url string, debug bool) error {
	return errors.New("native window is Windows-only")
}
func focusNativeWindow() bool { return false }
func closeNativeWindow() bool { return false }
