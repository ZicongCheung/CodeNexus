//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/ZicongCheung/CodeNexus/internal/assets"
)

// The tray is a hand-rolled Win32 implementation rather than a GUI library.
// The only Windows API surface it needs is a hidden window, a message loop and
// Shell_NotifyIcon, all of which arrive through x/sys (already a dependency),
// so the build stays free of third-party runtime code and other platforms keep
// compiling with CGO_ENABLED=0.

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	// GetModuleHandleW is a kernel32 entry point; asking user32 for it panics
	// inside x/sys before the tray ever gets a window.
	procGetModuleHandleW         = kernel32.NewProc("GetModuleHandleW")
	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procLoadCursorW              = user32.NewProc("LoadCursorW")
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procAppendMenuW              = user32.NewProc("AppendMenuW")
	procTrackPopupMenu           = user32.NewProc("TrackPopupMenu")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon              = user32.NewProc("DestroyIcon")
	procMessageBoxW              = user32.NewProc("MessageBoxW")
	procShellNotifyIconW         = shell32.NewProc("Shell_NotifyIconW")
)

const (
	wsOverlapped = 0x00000000
	wmDestroy    = 0x0002
	wmNull       = 0x0000
	wmApp        = 0x8000

	trayCallbackMsg = wmApp + 1

	wmLButtonDown   = 0x0201
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205

	nimAdd    = 0x00000000
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	mfString = 0x00000000
	mfGrayed = 0x00000001
	mfSep    = 0x00000800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	idcArrow = 32512

	iconVersion30000 = 0x00030000

	mbOK            = 0x00000000
	mbIconWarning   = 0x00000030
	mbSetForeground = 0x00010000
	mbTopmost       = 0x00040000
)

type wndClassExW struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSmall  windows.Handle
}

type notifyIconDataW struct {
	Size            uint32
	Wnd             windows.Handle
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            windows.Handle
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GuidItem        windows.GUID
	BalloonIcon     windows.Handle
}

type pointT struct {
	X int32
	Y int32
}

type msgT struct {
	Wnd     windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      pointT
}

// trayActions is what the menu does when an item is chosen.
type trayActions struct {
	ConsoleURL string
	ConfigPath string
	LogPath    string
	Dir        string
	StatusText func() string
	Quit       func()
}

var (
	trayWnd    windows.Handle
	trayIcon   windows.Handle
	trayData   *notifyIconDataW
	trayState  trayActions
	trayThread uint32
)

// reportTrayFailure tells the operator that the windowed build is running
// without its only visible surface. Without this the process is invisible: no
// console, no icon, no window, and the log it does write lives in the data
// directory nobody has a reason to open yet.
//
// A console build already shows the same warning in its log, where a modal box
// would just be in the way.
func reportTrayFailure(err error, consoleAddr, logPath string) {
	if hasConsole() {
		return
	}
	text := fmt.Sprintf(
		"无法创建托盘图标，CodeNexus 已在后台继续运行。\n\n"+
			"面板：%s\n"+
			"日志：%s\n\n"+
			"原因：%v",
		consoleAddr, logPath, err,
	)
	title, terr := windows.UTF16PtrFromString("CodeNexus")
	body, berr := windows.UTF16PtrFromString(text)
	if terr != nil || berr != nil {
		return
	}
	procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(body)),
		uintptr(unsafe.Pointer(title)),
		mbOK|mbIconWarning|mbSetForeground|mbTopmost,
	)
}

// startTray puts the icon in the notification area and runs its message loop on
// a dedicated OS thread, which is what Win32 requires for window ownership. It
// blocks, so callers run it in its own goroutine.
func startTray(actions trayActions) error {
	trayState = actions

	iconData, _, ok := assets.IconFromICO(assets.Icon, 32)
	if !ok {
		return fmt.Errorf("tray icon: the embedded icon has no usable entry")
	}
	hicon, _, err := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&iconData[0])),
		uintptr(len(iconData)),
		1, // fIcon
		iconVersion30000,
		0, 0, // let Windows pick from the payload's own size
		0, // LR_DEFAULTCOLOR
	)
	if hicon == 0 {
		return fmt.Errorf("tray icon: CreateIconFromResourceEx failed: %v", err)
	}
	trayIcon = windows.Handle(hicon)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := createTrayWindow(); err != nil {
		return err
	}
	if err := addTrayIcon(); err != nil {
		return err
	}
	messageLoop()
	return nil
}

func createTrayWindow() error {
	instance, _, _ := procGetModuleHandleW.Call(0)
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	className, _ := windows.UTF16PtrFromString("CodeNexusTrayWindow")

	wc := wndClassExW{
		Size:      uint32(unsafe.Sizeof(wndClassExW{})),
		WndProc:   windows.NewCallback(trayWindowProc),
		Instance:  windows.Handle(instance),
		Cursor:    windows.Handle(cursor),
		ClassName: className,
	}
	if ret, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		return fmt.Errorf("tray: RegisterClassEx failed: %v", err)
	}
	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		0,
		wsOverlapped,
		0, 0, 0, 0,
		0, 0,
		instance,
		0,
	)
	if hwnd == 0 {
		return fmt.Errorf("tray: CreateWindowEx failed: %v", err)
	}
	trayWnd = windows.Handle(hwnd)
	return nil
}

func addTrayIcon() error {
	data := notifyIconDataW{
		Wnd:             trayWnd,
		ID:              1,
		Flags:           nifMessage | nifIcon | nifTip,
		CallbackMessage: trayCallbackMsg,
		Icon:            trayIcon,
	}
	data.Size = uint32(unsafe.Sizeof(data))
	copyTip(&data, "CodeNexus")
	if ret, _, err := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&data))); ret == 0 {
		return fmt.Errorf("tray: Shell_NotifyIcon(NIM_ADD) failed: %v", err)
	}
	trayData = &data
	return nil
}

func copyTip(data *notifyIconDataW, text string) {
	tip, err := windows.UTF16FromString(text)
	if err != nil {
		return
	}
	if len(tip) > len(data.Tip) {
		tip = tip[:len(data.Tip)]
		tip[len(tip)-1] = 0
	}
	copy(data.Tip[:], tip)
}

func messageLoop() {
	var msg msgT
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
	if trayData != nil {
		procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(trayData)))
	}
	if trayIcon != 0 {
		procDestroyIcon.Call(uintptr(trayIcon))
	}
	if trayWnd != 0 {
		procDestroyWindow.Call(uintptr(trayWnd))
	}
}

func trayWindowProc(hwnd windows.Handle, msg uint32, wparam, lparam uintptr) uintptr {
	switch msg {
	case trayCallbackMsg:
		switch uint32(lparam) {
		case wmLButtonDblClk, wmLButtonUp:
			// A single click opens the console too: it is what most people try
			// first, and there is nothing else for the icon to do.
			openConsole()
		case wmRButtonUp:
			showTrayMenu(hwnd)
		}
		return 0
	case wmDestroy:
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
	return ret
}

func showTrayMenu(hwnd windows.Handle) {
	const (
		cmdOpen   = 1
		cmdStatus = 2
		cmdLog    = 3
		cmdConfig = 4
		cmdQuit   = 5
	)
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	appendItem(menu, cmdOpen, "打开面板")
	if trayState.StatusText != nil {
		// The status line is information; it is greyed so it cannot be picked.
		appendItemFlags(menu, mfString|mfGrayed, cmdStatus, trayState.StatusText())
	}
	appendItem(menu, cmdLog, "打开日志")
	appendItem(menu, cmdConfig, "打开配置目录")
	appendSep(menu)
	appendItem(menu, cmdQuit, "退出")

	var pt pointT
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Without this the menu stays on screen after a click elsewhere: a
	// well-known TrackPopupMenu requirement, not a stylistic choice.
	procSetForegroundWindow.Call(uintptr(hwnd))
	ret, _, _ := procTrackPopupMenu.Call(
		menu,
		tpmRightButton|tpmReturnCmd,
		uintptr(pt.X), uintptr(pt.Y),
		0, uintptr(hwnd), 0,
	)
	procPostMessageW.Call(uintptr(hwnd), wmNull, 0, 0)

	switch ret {
	case cmdOpen:
		openConsole()
	case cmdLog:
		openPath(trayState.LogPath)
	case cmdConfig:
		openPath(trayState.ConfigPath)
	case cmdQuit:
		if trayState.Quit != nil {
			trayState.Quit()
		}
	}
}

func appendItem(menu uintptr, id uint32, text string) {
	appendItemFlags(menu, mfString, id, text)
}

// appendItemFlags adds an entry with explicit flags, which is how the status
// line is shown as greyed text rather than an action.
func appendItemFlags(menu uintptr, flags uintptr, id uint32, text string) {
	label, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	procAppendMenuW.Call(menu, flags, uintptr(id), uintptr(unsafe.Pointer(label)))
}

func appendSep(menu uintptr) {
	procAppendMenuW.Call(menu, mfSep, 0, 0)
}

func openConsole() {
	if trayState.ConsoleURL != "" {
		_ = openBrowser(trayState.ConsoleURL)
	}
}

// openPath reveals a file or directory in Explorer. A missing file falls back
// to its directory, so the menu item always lands somewhere useful.
func openPath(path string) {
	if path == "" {
		path = trayState.Dir
	}
	if path == "" {
		return
	}
	_ = exec.Command("explorer.exe", path).Start()
}
