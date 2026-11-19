//go:build windows

package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	createNoWindow  = 0x08000000
	detachedProcess = 0x00000008

	wmApp        = 0x8000
	wmTrayIcon   = wmApp + 1
	wmCommand    = 0x0111
	wmDestroy    = 0x0002
	wmLButtonDbl = 0x0203
	wmRButtonUp  = 0x0205

	nimAdd    = 0x00000000
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	mfString    = 0x00000000
	mfSeparator = 0x00000800

	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020

	idiApplication = 32512

	cmdOpenHome = 1001
	cmdOpenSys  = 1002
	cmdShutDown = 1003
)

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	shell32                 = syscall.NewLazyDLL("shell32.dll")
	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procLoadIconW           = user32.NewProc("LoadIconW")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procShellNotifyIconW    = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type point struct {
	x int32
	y int32
}

type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type notifyIconDataW struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

func isServiceHealthy(port string) bool {
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz", port))
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// maybeDetachBackground relaunches Ark without a console window on Windows so the user
// does not need to keep a terminal window open. Returns true if the caller should exit.
func maybeDetachBackground(port string, args []string, openUI bool) bool {
	if isServiceHealthy(port) {
		if openUI {
			openBrowser(fmt.Sprintf("http://localhost:%s/", port))
		}
		return true
	}

	exePath, err := os.Executable()
	if err != nil || strings.TrimSpace(exePath) == "" {
		return false
	}

	childArgs := make([]string, 0, len(args)+1)
	childArgs = append(childArgs, "-background-child")
	childArgs = append(childArgs, args...)

	cmd := exec.Command(exePath, childArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNoWindow | detachedProcess,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	return true
}

// startTrayIcon displays a Windows System Tray icon so the background Ark service
// can be opened or shut down cleanly (stopping all Docker containers) without a console window.
func startTrayIcon(port string, onShutdown func()) func() {
	var (
		mu   sync.Mutex
		hwnd uintptr
	)

	ready := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		hInst, _, _ := procGetModuleHandleW.Call(0)
		className, _ := syscall.UTF16PtrFromString("ArkCommandCenterTrayClass")

		var shutdownOnce sync.Once
		triggerShutdown := func() {
			shutdownOnce.Do(onShutdown)
		}

		wndProc := syscall.NewCallback(func(h uintptr, msg uint32, wParam, lParam uintptr) uintptr {
			switch msg {
			case wmTrayIcon:
				if lParam == wmLButtonDbl {
					openBrowser(fmt.Sprintf("http://localhost:%s/", port))
					return 0
				}
				if lParam == wmRButtonUp {
					hMenu, _, _ := procCreatePopupMenu.Call()
					if hMenu != 0 {
						lblOpen, _ := syscall.UTF16PtrFromString("Open Ark Home")
						lblSys, _ := syscall.UTF16PtrFromString("System & Containers")
						lblStop, _ := syscall.UTF16PtrFromString("Shut Down Ark (Stop Containers)")
						_, _, _ = procAppendMenuW.Call(hMenu, mfString, cmdOpenHome, uintptr(unsafe.Pointer(lblOpen)))
						_, _, _ = procAppendMenuW.Call(hMenu, mfString, cmdOpenSys, uintptr(unsafe.Pointer(lblSys)))
						_, _, _ = procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
						_, _, _ = procAppendMenuW.Call(hMenu, mfString, cmdShutDown, uintptr(unsafe.Pointer(lblStop)))

						var pt point
						_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
						_, _, _ = procSetForegroundWindow.Call(h)
						_, _, _ = procTrackPopupMenu.Call(hMenu, tpmRightButton|tpmBottomAlign, uintptr(pt.x), uintptr(pt.y), 0, h, 0)
						_, _, _ = procDestroyMenu.Call(hMenu)
					}
					return 0
				}
			case wmCommand:
				switch wParam & 0xffff {
				case cmdOpenHome:
					openBrowser(fmt.Sprintf("http://localhost:%s/", port))
				case cmdOpenSys:
					openBrowser(fmt.Sprintf("http://localhost:%s/system", port))
				case cmdShutDown:
					triggerShutdown()
				}
				return 0
			case wmDestroy:
				_, _, _ = procPostQuitMessage.Call(0)
				return 0
			}
			ret, _, _ := procDefWindowProcW.Call(h, uintptr(msg), wParam, lParam)
			return ret
		})

		wc := wndClassExW{
			cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
			lpfnWndProc:   wndProc,
			hInstance:     hInst,
			lpszClassName: className,
		}
		_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

		winTitle, _ := syscall.UTF16PtrFromString("Ark Tray")
		h, _, _ := procCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(className)),
			uintptr(unsafe.Pointer(winTitle)),
			0, 0, 0, 0, 0,
			0, 0, hInst, 0,
		)
		if h == 0 {
			close(ready)
			return
		}

		mu.Lock()
		hwnd = h
		mu.Unlock()

		hIcon, _, _ := procLoadIconW.Call(0, idiApplication)
		var nid notifyIconDataW
		nid.cbSize = uint32(unsafe.Sizeof(nid))
		nid.hWnd = h
		nid.uID = 1
		nid.uFlags = nifMessage | nifIcon | nifTip
		nid.uCallbackMessage = wmTrayIcon
		nid.hIcon = hIcon
		tipUTF16, _ := syscall.UTF16FromString(fmt.Sprintf("Ark Command Center (http://localhost:%s)", port))
		copy(nid.szTip[:], tipUTF16)

		_, _, _ = procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
		close(ready)

		var m winMsg
		for {
			ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(ret) <= 0 {
				break
			}
			_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}

		_, _, _ = procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	}()

	<-ready
	return func() {
		mu.Lock()
		h := hwnd
		mu.Unlock()
		if h != 0 {
			_, _, _ = procPostMessageW.Call(h, wmDestroy, 0, 0)
		}
	}
}
