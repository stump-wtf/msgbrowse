//go:build desktop && darwin

// macOS platform glue for the About panel and the standard window-management
// menu actions (issue #429). The app menu is built in Go (shell.menu) so the
// About item can carry the Cmd+, accelerator and the version/tool content
// assembled in about.go; its standard siblings (Hide, Hide Others, Show All)
// need real NSApplication selectors, which Go-built Wails menu items cannot
// target — so this cgo seam wraps the four calls. Everything dispatches
// through the main queue, so any goroutine may call these safely.
//
// @joestump-agent 09/04/2026 - Added with #429: native About panel
// (Cmd+, / app menu) plus the selector glue the hand-built app menu needs.
package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa

// Definitions live in about_platform_darwin.m — a file that uses //export must
// keep its preamble to declarations only (cgo rule).
void msgbrowse_show_about_panel(const char* title, const char* message);
void msgbrowse_hide_app(void);
void msgbrowse_hide_others(void);
void msgbrowse_show_all(void);
*/
import "C"

import "unsafe"

// showAboutPanel presents the standard-looking About alert with the content
// assembled from about.go (version, build, verified tool versions).
func showAboutPanel(title, message string) {
	ctitle := C.CString(title)
	defer C.free(unsafe.Pointer(ctitle))
	cmsg := C.CString(message)
	defer C.free(unsafe.Pointer(cmsg))
	C.msgbrowse_show_about_panel(ctitle, cmsg)
}

// hideApp maps the app menu's Hide item to [NSApp hide:] (Cmd+H).
func hideApp() { C.msgbrowse_hide_app() }

// hideOthers maps Hide Others to [NSApp hideOtherApplications:] (⌥⌘H).
func hideOthers() { C.msgbrowse_hide_others() }

// showAll maps Show All to [NSApp unhideAllApplications:].
func showAll() { C.msgbrowse_show_all() }
