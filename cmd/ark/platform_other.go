//go:build !windows

package main

// Non-Windows builds do not need Win32 console-close hooks or a system tray icon.
func registerConsoleCloseHandler(_ func(), _ <-chan struct{}) {}

func maybeDetachBackground(_ string, _ []string, _ bool) bool { return false }

func startTrayIcon(_ string, _ func()) func() { return func() {} }
