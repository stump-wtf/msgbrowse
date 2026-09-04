//go:build desktop && !darwin

// Non-macOS stubs for the About-panel seam (issue #429): the app-menu About
// item is only built on darwin (shell.menu), so these never run — they exist
// so the desktop module still compiles headlessly on Linux, where the
// headless test suite (make desktop-test) runs.
//
// @joestump-agent 09/04/2026 - Added with #429.
package main

func showAboutPanel(string, string) {}
func hideApp()                      {}
func hideOthers()                   {}
func showAll()                      {}
