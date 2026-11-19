//go:build windows

package main

import (
	"log"
	"sync"
	"syscall"
	"time"
)

const (
	ctrlCloseEvent    = 2
	ctrlLogoffEvent   = 5
	ctrlShutdownEvent = 6
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
)

// registerConsoleCloseHandler intercepts Windows console close, logoff, and shutdown
// events (such as clicking the console window's X button) so Ark can gracefully stop
// Docker containers and record its shutdown timestamp before Windows terminates it.
func registerConsoleCloseHandler(trigger func(), done <-chan struct{}) {
	var once sync.Once
	cb := syscall.NewCallback(func(ctrlType uintptr) uintptr {
		switch ctrlType {
		case ctrlCloseEvent, ctrlLogoffEvent, ctrlShutdownEvent:
			log.Printf("[Ark] Windows console close event (%d) received, shutting down service and containers...", ctrlType)
			once.Do(trigger)
			select {
			case <-done:
			case <-time.After(4500 * time.Millisecond):
			}
			return 1
		default:
			return 0
		}
	})
	_, _, _ = procSetConsoleCtrlHandler.Call(cb, 1)
}
