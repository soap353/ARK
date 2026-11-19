//go:build !windows

package main

func maybeDetachBackground(port string, args []string, openUI bool) bool {
	return false
}

func startTrayIcon(port string, onShutdown func()) func() {
	return func() {}
}
