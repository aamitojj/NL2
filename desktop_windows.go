//go:build windows

package main

// Native desktop shell for Windows: a WebView2 window (github.com/jchv/go-webview2,
// pure Go, no cgo), single-instance handling, window placement memory, the
// "WebView2 missing" dialog and logging for the GUI subsystem.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
)

// The WebView2 window must be created and pumped on the main OS thread.
func init() { runtime.LockOSThread() }

var (
	user32                    = windows.NewLazySystemDLL("user32.dll")
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procMessageBoxW           = user32.NewProc("MessageBoxW")
	procSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	procShowWindow            = user32.NewProc("ShowWindow")
	procIsIconic              = user32.NewProc("IsIconic")
	procGetWindowPlacement    = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement    = user32.NewProc("SetWindowPlacement")
	procSetWindowLongPtrW     = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProcW       = user32.NewProc("CallWindowProcW")
	procAllowSetForegroundWin = user32.NewProc("AllowSetForegroundWindow")
	procMonitorFromRect       = user32.NewProc("MonitorFromRect")
	procPostMessageW          = user32.NewProc("PostMessageW")
	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procSetProcessDPIAwareCtx = user32.NewProc("SetProcessDpiAwarenessContext")
	procFindWindowW           = user32.NewProc("FindWindowW")
	procDestroyWindow         = user32.NewProc("DestroyWindow")
)

const (
	mbYesNoCancel   = 0x00000003
	mbOK            = 0x00000000
	mbIconWarning   = 0x00000030
	mbIconInfo      = 0x00000040
	mbSetForeground = 0x00010000
	idYes           = 6
	idNo            = 7
	swRestore       = 9
	swShowMax       = 3
	swShowNormal    = 1
	wmClose         = 0x0010
	gwlpWndProc     = ^uintptr(3) // -4
)

type winPoint struct{ X, Y int32 }
type winRect struct{ Left, Top, Right, Bottom int32 }
type windowPlacement struct {
	Length        uint32
	Flags         uint32
	ShowCmd       uint32
	PtMinPosition winPoint
	PtMaxPosition winPoint
	RcNormalPos   winRect
}

var (
	nativeMu   sync.Mutex
	nativeWV   webview2.WebView
	nativeHWND uintptr
	oldWndProc uintptr
	mutexH     windows.Handle
)

func isNativePlatform() bool { return true }

// hasConsole reports whether the process has a console (started from cmd or
// built without -H=windowsgui). Without one, stdout goes nowhere.
func hasConsole() bool {
	if procGetConsoleWindow.Find() != nil {
		return false
	}
	h, _, _ := procGetConsoleWindow.Call()
	return h != 0
}

// platformInit sets DPI awareness and redirects logs to a file for the GUI build.
func platformInit() {
	if procSetProcessDPIAwareCtx.Find() == nil {
		procSetProcessDPIAwareCtx.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
	}
	if hasConsole() {
		return
	}
	dir := appDataLocal()
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "dilreet.log")
	if st, err := os.Stat(p); err == nil && st.Size() > 2<<20 {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	log.SetOutput(f)
	os.Stdout, os.Stderr = f, f
	logFilePath = p
}

func appDataLocal() string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "Dilreet")
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "Dilreet")
	}
	return ""
}

// acquireInstanceLock takes the per-user single-instance mutex. wait > 0 keeps
// trying (used right after a self-update while the old process exits).
func acquireInstanceLock(wait time.Duration) bool {
	name, _ := windows.UTF16PtrFromString(`Local\Dilreet-SingleInstance-v1`)
	deadline := time.Now().Add(wait)
	for {
		h, err := windows.CreateMutex(nil, false, name)
		if err == nil {
			mutexH = h
			return true
		}
		if h != 0 {
			windows.CloseHandle(h)
		}
		if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return true // cannot tell; do not block startup
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func allowForeground(pid int) {
	if procAllowSetForegroundWin.Find() == nil {
		procAllowSetForegroundWin.Call(uintptr(pid))
	}
}

func messageBox(title, text string, flags uintptr) int {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	r, _, _ := procMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), flags|mbSetForeground)
	return int(r)
}

func showInfo(text string) { messageBox("Dilreet", text, mbOK|mbIconInfo) }

// webView2Status returns the installed WebView2 runtime version ("" if none).
func webView2Status() (string, error) { return webviewloader.GetInstalledVersion() }

// askNoWebView2 explains that WebView2 is missing. Returns "download",
// "browser" or "quit".
func askNoWebView2(detail string) string {
	msg := "Dilreet shows its own window with the Microsoft Edge WebView2 Runtime, which is not installed on this PC" +
		detail + ".\n\nIt is free from Microsoft and already included in Windows 11.\n\n" +
		"Yes  -  download WebView2 from Microsoft now (start Dilreet again afterwards).\n" +
		"No  -  open Dilreet in your web browser instead.\n" +
		"Cancel  -  quit."
	switch messageBox("Dilreet - WebView2 needed", msg, mbYesNoCancel|mbIconWarning) {
	case idYes:
		return "download"
	case idNo:
		return "browser"
	}
	return "quit"
}

// ---------- window placement ----------

type savedPlacement struct {
	Left, Top, Right, Bottom int32
	Maximized                bool
}

func placementPath() string {
	d, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "window.json")
}

func loadPlacement() *savedPlacement {
	b, err := os.ReadFile(placementPath())
	if err != nil {
		return nil
	}
	var p savedPlacement
	if json.Unmarshal(b, &p) != nil || p.Right-p.Left < 400 || p.Bottom-p.Top < 300 {
		return nil
	}
	return &p
}

func savePlacement(hwnd uintptr) {
	wp := windowPlacement{Length: uint32(unsafe.Sizeof(windowPlacement{}))}
	if r, _, _ := procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&wp))); r == 0 {
		return
	}
	p := savedPlacement{wp.RcNormalPos.Left, wp.RcNormalPos.Top, wp.RcNormalPos.Right, wp.RcNormalPos.Bottom, wp.ShowCmd == swShowMax}
	b, _ := json.Marshal(p)
	path := placementPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, b, 0o600)
}

func applyPlacement(hwnd uintptr, p *savedPlacement) {
	rc := winRect{p.Left, p.Top, p.Right, p.Bottom}
	// Only restore if the rectangle is still on a connected monitor.
	if m, _, _ := procMonitorFromRect.Call(uintptr(unsafe.Pointer(&rc)), 0 /*MONITOR_DEFAULTTONULL*/); m == 0 {
		return
	}
	wp := windowPlacement{Length: uint32(unsafe.Sizeof(windowPlacement{})), ShowCmd: swShowNormal, RcNormalPos: rc}
	if p.Maximized {
		wp.ShowCmd = swShowMax
	}
	procSetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&wp)))
}

// subclassProc saves the placement before the library's WM_CLOSE handler
// destroys the window.
func subclassProc(hwnd, msg, wp, lp uintptr) uintptr {
	if msg == wmClose {
		savePlacement(hwnd)
	}
	r, _, _ := procCallWindowProcW.Call(oldWndProc, hwnd, msg, wp, lp)
	return r
}

// runNativeWindow opens the Dilreet window and blocks until it is closed.
func runNativeWindow(url string, debug bool) error {
	dataDir := filepath.Join(appDataLocal(), "WebView2")
	p := loadPlacement()
	w, h := uint(1280), uint(840)
	if p != nil {
		w, h = uint(p.Right-p.Left), uint(p.Bottom-p.Top)
	}
	wv := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     debug,
		AutoFocus: true,
		DataPath:  dataDir,
		WindowOptions: webview2.WindowOptions{
			Title:  "Dilreet",
			Width:  w,
			Height: h,
			IconId: 1, // icon resource from rsrc_windows_amd64.syso (go-winres)
			Center: true,
		},
	})
	if wv == nil {
		// The library already showed an empty window before embedding failed.
		cls, _ := windows.UTF16PtrFromString("webview")
		title, _ := windows.UTF16PtrFromString("Dilreet")
		if h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title))); h != 0 {
			procDestroyWindow.Call(h)
		}
		return errors.New("could not create the WebView2 window")
	}
	hwnd := uintptr(wv.Window())
	nativeMu.Lock()
	nativeWV, nativeHWND = wv, hwnd
	nativeMu.Unlock()
	wv.SetSize(900, 600, webview2.HintMin)
	if p != nil {
		applyPlacement(hwnd, p)
	}
	oldWndProc, _, _ = procSetWindowLongPtrW.Call(hwnd, gwlpWndProc, syscall.NewCallback(subclassProc))
	wv.Navigate(url)
	wv.Run()
	nativeMu.Lock()
	nativeWV = nil
	nativeMu.Unlock()
	return nil
}

// focusNativeWindow brings the existing window to the front. Returns false
// when there is no native window (browser mode).
func focusNativeWindow() bool {
	nativeMu.Lock()
	wv, hwnd := nativeWV, nativeHWND
	nativeMu.Unlock()
	if wv == nil {
		return false
	}
	wv.Dispatch(func() {
		if r, _, _ := procIsIconic.Call(hwnd); r != 0 {
			procShowWindow.Call(hwnd, swRestore)
		}
		procSetForegroundWindow.Call(hwnd)
	})
	return true
}

// closeNativeWindow closes the window like the user clicking X (placement is
// saved, Run returns, main shuts down).
func closeNativeWindow() bool {
	nativeMu.Lock()
	wv, hwnd := nativeWV, nativeHWND
	nativeMu.Unlock()
	if wv == nil {
		return false
	}
	procPostMessageW.Call(hwnd, wmClose, 0, 0)
	return true
}

func nativeDebugInfo() string { return fmt.Sprintf("hwnd=%#x", nativeHWND) }
