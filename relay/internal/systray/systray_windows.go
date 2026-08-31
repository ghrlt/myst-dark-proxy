//go:build windows

package systray

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// A note on pointers. LazyProc.Call takes uintptr arguments, so a pointer that
// exists only as one of them is a pointer the garbage collector cannot see.
// Every address handed to Win32 below therefore comes from a named local, kept
// alive across the call, rather than from a temporary built inside the argument
// list.

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassEx     = user32.NewProc("RegisterClassExW")
	procCreateWindowEx      = user32.NewProc("CreateWindowExW")
	procDefWindowProc       = user32.NewProc("DefWindowProcW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procGetMessage          = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessage     = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procPostMessage         = user32.NewProc("PostMessageW")
	procRegisterWindowMsg   = user32.NewProc("RegisterWindowMessageW")
	procLoadCursor          = user32.NewProc("LoadCursorW")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenu          = user32.NewProc("AppendMenuW")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procSetMenuDefaultItem  = user32.NewProc("SetMenuDefaultItem")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procMessageBox          = user32.NewProc("MessageBoxW")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procGetDC               = user32.NewProc("GetDC")
	procReleaseDC           = user32.NewProc("ReleaseDC")
	procCreateIconIndirect  = user32.NewProc("CreateIconIndirect")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procShellNotifyIcon     = shell32.NewProc("Shell_NotifyIconW")
	procCreateDIBSection    = gdi32.NewProc("CreateDIBSection")
	procCreateBitmap        = gdi32.NewProc("CreateBitmap")
	procDeleteObject        = gdi32.NewProc("DeleteObject")
	procGetModuleHandle     = kernel32.NewProc("GetModuleHandleW")
	procFreeConsole         = kernel32.NewProc("FreeConsole")
	procConsoleProcessList  = kernel32.NewProc("GetConsoleProcessList")
)

const (
	wmDestroy      = 0x0002
	wmClose        = 0x0010
	wmNull         = 0x0000
	wmLButtonUp    = 0x0202
	wmLButtonDbl   = 0x0203
	wmRButtonUp    = 0x0205
	wmContextMenu  = 0x007B
	wmApp          = 0x8000
	wmTrayCallback = wmApp + 1 // the notification area talking to us
	wmRefresh      = wmApp + 2 // "re-read the wanted icon", from any goroutine
	wmBalloon      = wmApp + 3 // "show the queued balloon"
	wmTeardown     = wmApp + 4 // "take the icon down and stop"
	wmRetryAdd     = wmApp + 5 // "the shell would not take the icon; try again"

	nimAdd    = 0x0
	nimModify = 0x1
	nimDelete = 0x2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10

	niifInfo    = 0x1
	niifWarning = 0x2

	mfString    = 0x000
	mfGrayed    = 0x001
	mfChecked   = 0x008
	mfPopup     = 0x010
	mfSeparator = 0x800

	tpmLeftAlign   = 0x0000
	tpmRightButton = 0x0002
	tpmNoNotify    = 0x0080
	tpmReturnCmd   = 0x0100

	wsOverlapped = 0x00000000
	idcArrow     = 32512
	smCXSmIcon   = 49
	smCYSmIcon   = 50
	dibRGBColors = 0

	mbOK              = 0x0000
	mbIconWarning     = 0x0030
	mbIconInformation = 0x0040
	mbSetForeground   = 0x00010000
	mbTopMost         = 0x00040000

	maxTipChars         = 127
	maxBalloonBodyChars = 255
	maxBalloonHeadChars = 63
)

type point struct{ X, Y int32 }

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type msg struct {
	Wnd     uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

// notifyIconData is NOTIFYICONDATAW. Go lays this out exactly as the C
// compiler does on amd64, so its size is the 976 bytes the shell expects and
// every field lands where it looks for it.
type notifyIconData struct {
	Size            uint32
	Wnd             uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GuidItem        [16]byte
	BalloonIcon     uintptr
}

type iconInfo struct {
	Icon     int32
	XHotspot uint32
	YHotspot uint32
	Mask     uintptr
	Colour   uintptr
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// --- the tray itself -------------------------------------------------------

// Tray is a live notification-area icon. Every method on it is safe to call
// from any goroutine: each records what is wanted and posts a message to the
// window, and the thread pumping that window is the only one that ever speaks
// to the shell.
type Tray struct {
	hwnd uintptr

	mu       sync.Mutex
	colour   Colour
	tooltip  string
	balloons []balloon
}

type balloon struct {
	title, body string
	warning     bool
}

// Set changes the icon's colour and hover text.
func (t *Tray) Set(c Colour, tooltip string) {
	t.mu.Lock()
	same := t.colour == c && t.tooltip == tooltip
	t.colour, t.tooltip = c, tooltip
	t.mu.Unlock()
	if same {
		return
	}
	t.post(wmRefresh)
}

// Notify raises a balloon over the icon. Windows may hold it back - focus
// assist and full-screen applications swallow them - so this is for things
// worth saying, not for things that must be heard.
func (t *Tray) Notify(title, body string, warning bool) {
	t.mu.Lock()
	t.balloons = append(t.balloons, balloon{title: title, body: body, warning: warning})
	t.mu.Unlock()
	t.post(wmBalloon)
}

// Quit takes the icon down and ends Run.
func (t *Tray) Quit() { t.post(wmTeardown) }

// queued reports whether anything is waiting to be said.
func (t *Tray) queued() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.balloons) > 0
}

func (t *Tray) post(message uint32) {
	if t.hwnd != 0 {
		procPostMessage.Call(t.hwnd, uintptr(message), 0, 0)
	}
}

// Show puts a block of text on screen. A message box is a plain thing, but it
// takes as many lines as the answer needs and it is always readable, which a
// balloon that the machine may decide to suppress is not.
func Show(title, body string) { dialog(title, body, mbOK|mbIconInformation) }

// Warn is Show with the icon that says something went wrong.
func Warn(title, body string) { dialog(title, body, mbOK|mbIconWarning) }

// dialog runs a message box on a thread of its own. Deliberately not the
// icon's thread: a modal dialog pumps its own messages, and one opened on the
// tray's thread would leave the icon unable to redraw or reopen its menu until
// it was dismissed.
func dialog(title, body string, style uintptr) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		head, text := utf16Ptr(title), utf16Ptr(body)
		procMessageBox.Call(0,
			uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(head)),
			style|mbSetForeground|mbTopMost)
		runtime.KeepAlive(head)
		runtime.KeepAlive(text)
	}()
	<-done
}

// trays maps a window to the state behind it. The window procedure is a C
// callback handed a window handle, not a Go pointer, so this is how it finds
// its way back.
var (
	traysMu sync.Mutex
	trays   = map[uintptr]*runState{}

	classOnce sync.Once
	classErr  error
	className *uint16

	wndProcCallback = syscall.NewCallback(wndProc)
	// taskbarCreated is broadcast when Explorer restarts. Icons added before
	// that are gone, and a tray that does not listen for it quietly disappears
	// for the rest of the session.
	taskbarCreated uint32
)

type runState struct {
	tray *Tray
	menu func() []Item
	// icons are cached by colour: each is a GDI object, and drawing a new one
	// on every status poll would leak them by the thousand.
	icons map[Colour]uintptr
	data  notifyIconData
	// added is whether the shell currently has the icon. Until it does there
	// is nothing to modify and no balloon to hang anywhere, and attempt counts
	// how many times it has been offered one.
	added   bool
	attempt int
}

// Run puts the icon up and pumps its messages until Quit is called.
//
// It locks its goroutine to a thread for its whole life because that is the
// rule the window belongs to: a window's messages may only be collected on the
// thread that created it.
func Run(opt Options) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := registerClass(); err != nil {
		return err
	}

	title := utf16Ptr(opt.Title)
	hwnd, _, err := procCreateWindowEx.Call(0,
		uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		wsOverlapped, 0, 0, 0, 0, 0, 0, instance(), 0)
	runtime.KeepAlive(title)
	if hwnd == 0 {
		return fmt.Errorf("create the window behind the icon: %w", err)
	}

	tray := &Tray{hwnd: hwnd, colour: opt.Colour, tooltip: opt.Tooltip}
	state := &runState{tray: tray, menu: opt.Menu, icons: map[Colour]uintptr{}}
	state.data = notifyIconData{
		Size:            uint32(unsafe.Sizeof(notifyIconData{})),
		Wnd:             hwnd,
		ID:              1,
		Flags:           nifMessage | nifIcon | nifTip,
		CallbackMessage: wmTrayCallback,
	}

	traysMu.Lock()
	trays[hwnd] = state
	traysMu.Unlock()
	defer func() {
		traysMu.Lock()
		delete(trays, hwnd)
		traysMu.Unlock()
		for _, icon := range state.icons {
			procDestroyIcon.Call(icon)
		}
	}()

	state.apply(nimAdd)
	if opt.Ready != nil {
		go opt.Ready(tray)
	}

	var m msg
	for {
		// GetMessage returns 0 for WM_QUIT and -1 for an error; either way
		// there is nothing further to pump.
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			return nil
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func registerClass() error {
	classOnce.Do(func() {
		className = utf16Ptr("MystRelayTray")
		cursor, _, _ := procLoadCursor.Call(0, idcArrow)
		class := wndClassEx{
			Size:      uint32(unsafe.Sizeof(wndClassEx{})),
			WndProc:   wndProcCallback,
			Instance:  instance(),
			Cursor:    cursor,
			ClassName: className,
		}
		atom, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&class)))
		runtime.KeepAlive(&class)
		if atom == 0 {
			classErr = fmt.Errorf("register the window class: %w", err)
			return
		}
		name := utf16Ptr("TaskbarCreated")
		id, _, _ := procRegisterWindowMsg.Call(uintptr(unsafe.Pointer(name)))
		runtime.KeepAlive(name)
		taskbarCreated = uint32(id)
	})
	return classErr
}

func instance() uintptr {
	h, _, _ := procGetModuleHandle.Call(0)
	return h
}

// wndProc is the window procedure. It runs on the thread inside Run, so what
// it touches is either that thread's own or behind the tray's mutex.
func wndProc(hwnd, message, wparam, lparam uintptr) uintptr {
	traysMu.Lock()
	state := trays[hwnd]
	traysMu.Unlock()
	if state == nil {
		ret, _, _ := procDefWindowProc.Call(hwnd, message, wparam, lparam)
		return ret
	}

	switch uint32(message) {
	case wmTrayCallback:
		// The low word of lParam is the mouse message the shell saw.
		switch uint32(lparam) & 0xFFFF {
		case wmRButtonUp, wmContextMenu, wmLButtonUp:
			state.showMenu()
		case wmLButtonDbl:
			state.runDefault()
		}
		return 0

	case wmRefresh:
		state.apply(nimModify)
		return 0

	case wmBalloon:
		state.showBalloon()
		return 0

	case wmRetryAdd:
		state.apply(nimAdd)
		return 0

	case wmTeardown, wmClose:
		state.added = false
		procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&state.data)))
		procDestroyWindow.Call(hwnd)
		return 0

	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}

	// Explorer restarted and took every icon with it; put ours back.
	if taskbarCreated != 0 && uint32(message) == taskbarCreated {
		state.added, state.attempt = false, 0
		state.apply(nimAdd)
		return 0
	}

	ret, _, _ := procDefWindowProc.Call(hwnd, message, wparam, lparam)
	return ret
}

// apply pushes the wanted colour and tooltip into the notification area.
//
// The operation asked for is what to do to an icon the shell already has;
// while it has none, every call is an add.
func (s *runState) apply(operation uintptr) {
	s.tray.mu.Lock()
	colour, tooltip := s.tray.colour, s.tray.tooltip
	s.tray.mu.Unlock()

	icon, ok := s.icons[colour]
	if !ok {
		// A failure is not cached: GDI running out of handles is a passing
		// condition, and remembering a zero would leave the icon blank for the
		// rest of the session.
		if icon = makeIcon(colour); icon != 0 {
			s.icons[colour] = icon
		}
	}
	s.data.Flags = nifMessage | nifIcon | nifTip
	s.data.Icon = icon
	putUTF16(s.data.Tip[:], tooltip, maxTipChars)
	if !s.added {
		operation = nimAdd
	}
	done, _, _ := procShellNotifyIcon.Call(operation, uintptr(unsafe.Pointer(&s.data)))

	// The answer is worth reading. It is how a shell that is not ready says
	// so, and the difference between an icon that turns up a moment late and
	// one that never turns up at all.
	if s.added = done != 0; !s.added {
		s.retryAdd()
		return
	}
	s.attempt = 0
	// Anything said while there was nowhere to say it is still queued.
	if s.tray.queued() {
		s.tray.post(wmBalloon)
	}
}

const (
	// addAttempts, addRetryEvery and addRetryMax bound the wait for a shell
	// that is not listening yet: the last attempt falls a little over four
	// minutes after the first. Long enough for a slow sign-in, and short
	// enough that a refusal for some other reason gives up rather than asking
	// for the rest of the session.
	addAttempts   = 20
	addRetryEvery = 2 * time.Second
	addRetryMax   = 15 * time.Second
)

// retryAdd asks for another go at putting the icon up, after a pause.
//
// The notification area is not there for the whole of a sign-in. A task with a
// logon trigger routinely starts before Explorer has finished building the
// taskbar, and Shell_NotifyIcon against a shell that is not listening yet
// simply answers false. TaskbarCreated covers Explorer restarting later but
// not this: a broadcast reaches the windows that exist when it is sent, so a
// window created moments after Explorer sent it - and moments before the tray
// would have accepted an icon - hears nothing, and sits there iconless for the
// rest of the session. Which is exactly what an autostarted relay looked like,
// while the same binary started by hand had an icon every time.
//
// The wait happens on a goroutine of its own because the thread that would
// otherwise do the waiting is the one pumping the window's messages: an icon
// that is late is a nuisance, but a menu that does not open is a broken one.
func (s *runState) retryAdd() {
	if s.attempt >= addAttempts {
		return
	}
	s.attempt++
	wait := min(time.Duration(s.attempt)*addRetryEvery, addRetryMax)
	hwnd := s.data.Wnd
	go func() {
		time.Sleep(wait)
		procPostMessage.Call(hwnd, wmRetryAdd, 0, 0)
	}()
}

// showBalloon raises the oldest queued balloon.
func (s *runState) showBalloon() {
	// With no icon there is nothing for one to appear over. Leave it queued:
	// the add that succeeds asks for it again.
	if !s.added {
		return
	}
	s.tray.mu.Lock()
	if len(s.tray.balloons) == 0 {
		s.tray.mu.Unlock()
		return
	}
	b := s.tray.balloons[0]
	s.tray.balloons = s.tray.balloons[1:]
	s.tray.mu.Unlock()

	data := s.data
	data.Flags = nifInfo
	data.InfoFlags = niifInfo
	if b.warning {
		data.InfoFlags = niifWarning
	}
	putUTF16(data.Info[:], b.body, maxBalloonBodyChars)
	putUTF16(data.InfoTitle[:], b.title, maxBalloonHeadChars)
	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&data)))
	runtime.KeepAlive(&data)
}

// showMenu builds the menu from scratch and runs whatever was chosen.
func (s *runState) showMenu() {
	items := s.items()
	if len(items) == 0 {
		return
	}
	hmenu, _, _ := procCreatePopupMenu.Call()
	if hmenu == 0 {
		return
	}
	// A menu destroyed takes its submenus with it, so the one handle here
	// releases everything the walk below created.
	defer procDestroyMenu.Call(hmenu)

	next := uintptr(0) // TrackPopupMenu returns 0 for "nothing chosen"
	appendItems(hmenu, items, &next)

	// The two calls either side of TrackPopupMenu are the documented way to
	// make a menu belonging to a window with no taskbar presence behave:
	// without the first it can open behind other windows, and without the
	// second it stays open after a click somewhere else.
	procSetForegroundWindow.Call(s.data.Wnd)
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	chosen, _, _ := procTrackPopupMenu.Call(hmenu,
		tpmLeftAlign|tpmRightButton|tpmReturnCmd|tpmNoNotify,
		uintptr(pt.X), uintptr(pt.Y), 0, s.data.Wnd, 0)
	procPostMessage.Call(s.data.Wnd, wmNull, 0, 0)

	flat := flatten(items)
	if chosen == 0 || int(chosen) > len(flat) {
		return
	}
	if do := flat[chosen-1].Do; do != nil {
		go do()
	}
}

// appendItems fills a menu, opening a submenu of its own for any item that has
// children, and marks that menu's own default item.
//
// Ids are handed out in the same depth-first order flatten walks in, so the id
// TrackPopupMenu returns names the same line there. An item that opens a
// submenu still takes an id it will never be clicked with - Win32 has nowhere
// to put one on a popup - which is what keeps the two walks in step.
func appendItems(hmenu uintptr, items []Item, next *uintptr) {
	var bold uintptr
	for _, item := range items {
		*next++
		id := *next
		if item.Separator {
			procAppendMenu.Call(hmenu, mfSeparator, 0, 0)
			continue
		}
		label := utf16Ptr(item.Label)

		if len(item.Children) > 0 {
			sub, _, _ := procCreatePopupMenu.Call()
			if sub == 0 {
				// The submenu cannot be drawn, but its ids are already spoken
				// for: step over them so everything after it keeps its own.
				*next += uintptr(len(flatten(item.Children)))
				continue
			}
			appendItems(sub, item.Children, next)
			flags := uintptr(mfString | mfPopup)
			if item.Disabled {
				flags |= mfGrayed
			}
			procAppendMenu.Call(hmenu, flags, sub, uintptr(unsafe.Pointer(label)))
			runtime.KeepAlive(label)
			continue
		}

		flags := uintptr(mfString)
		if item.Disabled {
			flags |= mfGrayed
		}
		if item.Checked {
			flags |= mfChecked
		}
		procAppendMenu.Call(hmenu, flags, id, uintptr(unsafe.Pointer(label)))
		runtime.KeepAlive(label)
		if item.Bold && bold == 0 {
			bold = id
		}
	}
	// A default belongs to the menu it was marked in, so an item marked inside
	// a submenu does not become the one a double-click on the icon runs.
	if bold != 0 {
		procSetMenuDefaultItem.Call(hmenu, bold, 0)
	}
}

// runDefault runs what a double-click on the icon means: the bold item.
func (s *runState) runDefault() {
	for _, item := range flatten(s.items()) {
		if item.Bold && item.Do != nil {
			go item.Do()
			return
		}
	}
}

func (s *runState) items() []Item {
	if s.menu == nil {
		return nil
	}
	return s.menu()
}

// --- drawing ---------------------------------------------------------------

// makeIcon turns the drawn disc into an HICON at whatever size this machine
// asks small icons to be.
func makeIcon(c Colour) uintptr {
	size := systemIconSize()
	// A device-independent bitmap wants the channels the other way round from
	// the way they are drawn.
	pixels := discPixels(size, c)
	for i := 0; i < len(pixels); i += 4 {
		pixels[i], pixels[i+2] = pixels[i+2], pixels[i]
	}

	header := bitmapInfoHeader{
		Size:  uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width: int32(size),
		// A negative height asks for a top-down bitmap, so row zero is the top
		// one and the buffer reads the way it was drawn.
		Height:   -int32(size),
		Planes:   1,
		BitCount: 32,
	}
	hdc, _, _ := procGetDC.Call(0)
	defer procReleaseDC.Call(0, hdc)

	var bits unsafe.Pointer
	colour, _, _ := procCreateDIBSection.Call(hdc,
		uintptr(unsafe.Pointer(&header)), dibRGBColors,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	runtime.KeepAlive(&header)
	if colour == 0 {
		return 0
	}
	defer procDeleteObject.Call(colour)
	copy(unsafe.Slice((*byte)(bits), len(pixels)), pixels)

	// The AND mask is ignored for a 32-bit icon carrying an alpha channel, but
	// CreateIconIndirect still wants one of the right shape. Scanlines in a
	// bitmap made this way are word-aligned, hence the rounding.
	maskBits := make([]byte, ((size+15)/16)*2*size)
	mask, _, _ := procCreateBitmap.Call(uintptr(size), uintptr(size), 1, 1,
		uintptr(unsafe.Pointer(&maskBits[0])))
	runtime.KeepAlive(maskBits)
	if mask == 0 {
		return 0
	}
	defer procDeleteObject.Call(mask)

	info := iconInfo{Icon: 1, Mask: mask, Colour: colour}
	icon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&info)))
	runtime.KeepAlive(&info)
	return icon
}

// systemIconSize is what this machine wants a small icon to be: 16 at the usual
// scaling, larger on a display that is scaled up.
func systemIconSize() int {
	w, _, _ := procGetSystemMetrics.Call(smCXSmIcon)
	h, _, _ := procGetSystemMetrics.Call(smCYSmIcon)
	size := max(int(w), int(h))
	if size < 16 || size > 64 {
		return 16
	}
	return size
}

// --- console ---------------------------------------------------------------

// DetachConsole closes the console window this process was given, but only when
// it is ours alone to close.
//
// A tray started by the scheduler or from a shortcut is handed a fresh console
// that would otherwise sit on the desktop for as long as the icon does. One
// started by hand shares the terminal it was typed into, and closing that would
// take the operator's prompt with it - so the process list is asked first, and
// a shared console is left alone.
func DetachConsole() {
	var pids [4]uint32
	n, _, _ := procConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	runtime.KeepAlive(&pids)
	if n == 1 {
		procFreeConsole.Call()
	}
}

// --- strings ---------------------------------------------------------------

func utf16Ptr(s string) *uint16 {
	encoded := append(utf16.Encode([]rune(sanitise(s))), 0)
	return &encoded[0]
}

// putUTF16 copies s into a fixed-width field, truncating to leave room for the
// terminator the shell expects.
func putUTF16(dst []uint16, s string, limit int) {
	limit = min(limit, len(dst)-1)
	encoded := utf16.Encode([]rune(sanitise(s)))
	if len(encoded) > limit {
		encoded = encoded[:limit]
	}
	n := copy(dst, encoded)
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
}

// sanitise removes NUL, which would truncate a string Windows reads as
// NUL-terminated at whatever point it appeared.
func sanitise(s string) string {
	for i := range len(s) {
		if s[i] == 0 {
			return s[:i]
		}
	}
	return s
}
