//go:build linux

package systray

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// The Linux notification area, which is a conversation rather than a window.
//
// A panel does not host our pixels here. We register an object with
// org.kde.StatusNotifierWatcher describing an icon - its colour as a bitmap,
// its tooltip, and the object path of a menu - and the panel draws all of it,
// asking us for the menu's contents when somebody clicks. So there is no
// message loop and nothing to paint: the whole implementation is answering
// questions about what the menu currently says.
//
// The protocol is StatusNotifierItem plus com.canonical.dbusmenu, which is what
// XFCE, KDE and GNOME-with-an-extension all listen for. The older XEmbed tray
// is not supported: it would mean drawing our own window, our own text and our
// own menu in raw X11, for desktops that have largely moved on.

const (
	sniPath     = "/StatusNotifierItem"
	sniIface    = "org.kde.StatusNotifierItem"
	menuPath    = "/MenuBar"
	menuIface   = "com.canonical.dbusmenu"
	watcherName = "org.kde.StatusNotifierWatcher"
	watcherPath = "/StatusNotifierWatcher"

	propsIface  = "org.freedesktop.DBus.Properties"
	introIface  = "org.freedesktop.DBus.Introspectable"
	peerIface   = "org.freedesktop.DBus.Peer"
	notifyName  = "org.freedesktop.Notifications"
	notifyPath  = "/org/freedesktop/Notifications"
	notifyIface = "org.freedesktop.Notifications"
)

// Tray is a live notification-area icon. Every method on it is safe to call
// from any goroutine.
type Tray struct {
	bus  *conn
	opts Options

	mu      sync.Mutex
	colour  Colour
	tooltip string
	// items is the menu as it was last published, and layout the revision the
	// panel knows about. A panel is entitled to cache the layout, so a change
	// has to be announced rather than merely waited for.
	items    []Item
	rendered string
	revision uint32

	quitOnce sync.Once
	quit     chan struct{}
}

// Set changes the icon's colour and hover text.
func (t *Tray) Set(c Colour, tooltip string) {
	t.mu.Lock()
	changedIcon := t.colour != c
	changedTip := t.tooltip != tooltip
	t.colour, t.tooltip = c, tooltip
	t.mu.Unlock()

	// The panel holds the old values until told they are stale; these signals
	// are what make it ask again.
	if changedIcon {
		_ = t.bus.emit(sniPath, sniIface, "NewIcon", "")
	}
	if changedTip {
		_ = t.bus.emit(sniPath, sniIface, "NewToolTip", "")
	}
	t.refreshMenu()
}

// Notify raises a desktop notification. The daemon may hold it back - a
// do-not-disturb mode will - so this is for things worth saying, not for things
// that must be heard.
func (t *Tray) Notify(title, body string, warning bool) {
	urgency := byte(1)
	if warning {
		urgency = 2
	}
	hints := map[string]variant{"urgency": {Sig: "y", V: urgency}}
	timeout := int32(8000)
	if warning {
		timeout = 15000
	}
	go func() {
		_, err := t.bus.call(notifyName, notifyPath, notifyIface, "Notify",
			"susssasa{sv}i",
			"myst-relay", uint32(0), "network-vpn", title, body,
			[]any{}, hints, timeout)
		if err != nil {
			// No notification daemon, or it refused. The icon and its menu
			// still say everything this would have.
			fmt.Fprintf(os.Stderr, "myst-relay: %s: %v\n", title, err)
		}
	}()
}

// Quit takes the icon down and ends Run.
func (t *Tray) Quit() { t.quitOnce.Do(func() { close(t.quit) }) }

// Run puts the icon up and serves it until Quit is called.
func Run(opt Options) error {
	bus, err := dial()
	if err != nil {
		return err
	}
	defer bus.Close()

	t := &Tray{
		bus:     bus,
		opts:    opt,
		colour:  opt.Colour,
		tooltip: opt.Tooltip,
		quit:    make(chan struct{}),
	}
	t.items = t.build()
	bus.onCall = t.dispatch

	// The name is what the watcher is handed, and what it comes back to. The
	// convention is the interface name, this process's id, and an instance
	// number for a second icon in the same process.
	name := fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", os.Getpid())
	if err := bus.requestName(name); err != nil {
		return err
	}
	if _, err := bus.call(watcherName, watcherPath, watcherName,
		"RegisterStatusNotifierItem", "s", name); err != nil {
		return fmt.Errorf("no notification area accepted the icon: %w\n"+
			"This desktop needs a StatusNotifierItem host - on XFCE that is the panel's\n"+
			"\"Status Notifier Plugin\" or a \"Notification Area\" set to support it", err)
	}

	if opt.Ready != nil {
		go opt.Ready(t)
	}

	select {
	case <-t.quit:
	case <-bus.Done():
		// The session bus went away, which means the desktop session did.
	}
	return nil
}

// --- the menu --------------------------------------------------------------

// build asks the caller what the menu says now.
func (t *Tray) build() []Item {
	if t.opts.Menu == nil {
		return nil
	}
	return t.opts.Menu()
}

// refreshMenu rebuilds the menu and tells the panel when it has changed.
//
// A panel is free to cache a layout until it is told otherwise, and some do not
// call AboutToShow at all, so the icon's own polling is what keeps the menu
// truthful: it is rebuilt on every status change and the revision only moves
// when the text actually differs.
func (t *Tray) refreshMenu() {
	items := t.build()
	rendered := render(items)

	t.mu.Lock()
	if rendered == t.rendered {
		t.mu.Unlock()
		return
	}
	t.items, t.rendered = items, rendered
	t.revision++
	revision := t.revision
	t.mu.Unlock()

	_ = t.bus.emit(menuPath, menuIface, "LayoutUpdated", "ui", revision, int32(0))
}

// render is the menu as one comparable string, so "has anything changed?" does
// not need a field-by-field walk of two trees.
func render(items []Item) string {
	var b strings.Builder
	for _, item := range items {
		switch {
		case item.Separator:
			b.WriteString("---\n")
		case item.Disabled:
			b.WriteString("- " + item.Label + "\n")
		default:
			b.WriteString("* " + item.Label + "\n")
		}
	}
	return b.String()
}

// snapshot returns the published menu and the revision it belongs to.
func (t *Tray) snapshot() ([]Item, uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.items, t.revision
}

// menuItem renders one item as a dbusmenu node: an id, its properties, and the
// children it does not have.
func menuItem(id int32, item Item) any {
	props := map[string]variant{}
	if item.Separator {
		props["type"] = vs("separator")
	} else {
		props["label"] = vs(item.Label)
		props["enabled"] = vb(!item.Disabled && item.Do != nil)
		props["visible"] = vb(true)
	}
	return []any{id, props, []any{}}
}

// layout is the whole menu in the shape GetLayout answers with: a root item
// whose children are the lines, each wrapped in a variant.
func (t *Tray) layout() (uint32, any) {
	items, revision := t.snapshot()
	children := make([]any, 0, len(items))
	for i, item := range items {
		children = append(children, variant{Sig: "(ia{sv}av)", V: menuItem(int32(i+1), item)})
	}
	root := []any{
		int32(0),
		map[string]variant{"children-display": vs("submenu")},
		children,
	}
	return revision, root
}

// activate runs the item behind an id, on a goroutine of its own so a menu
// action that opens a dialog or waits on the network does not hold up the bus.
func (t *Tray) activate(id int32) {
	items, _ := t.snapshot()
	i := int(id) - 1
	if i < 0 || i >= len(items) {
		return
	}
	if do := items[i].Do; do != nil && !items[i].Disabled {
		go do()
	}
}

// activateDefault runs what a click on the icon itself means: the item the menu
// marks as its default.
func (t *Tray) activateDefault() {
	items, _ := t.snapshot()
	for _, item := range items {
		if item.Bold && item.Do != nil {
			go item.Do()
			return
		}
	}
}

// --- answering the panel ---------------------------------------------------

// dispatch routes an incoming method call. It runs on the connection's read
// goroutine, so every branch answers immediately and hands real work away.
func (t *Tray) dispatch(c *conn, m *message) {
	switch m.Interface {
	case peerIface:
		if m.Member == "Ping" {
			c.reply(m, "")
			return
		}
		if m.Member == "GetMachineId" {
			c.reply(m, "s", machineID())
			return
		}

	case introIface:
		c.reply(m, "s", introspection(m.Path))
		return

	case propsIface:
		t.properties(c, m)
		return

	case sniIface:
		switch m.Member {
		case "Activate", "SecondaryActivate":
			t.activateDefault()
			c.reply(m, "")
			return
		case "ContextMenu", "Scroll":
			// The panel draws the menu itself from the Menu property; there is
			// nothing for us to pop up.
			c.reply(m, "")
			return
		}

	case menuIface:
		t.menuCall(c, m)
		return
	}
	c.replyError(m, "org.freedesktop.DBus.Error.UnknownMethod",
		"myst-relay does not implement "+m.Interface+"."+m.Member)
}

// properties answers Get, GetAll and Set for both exported objects.
func (t *Tray) properties(c *conn, m *message) {
	args, err := m.Args()
	if err != nil {
		c.replyError(m, "org.freedesktop.DBus.Error.InvalidArgs", err.Error())
		return
	}
	switch m.Member {
	case "Get":
		if len(args) < 2 {
			c.replyError(m, "org.freedesktop.DBus.Error.InvalidArgs", "Get takes an interface and a name")
			return
		}
		iface, _ := args[0].(string)
		name, _ := args[1].(string)
		v, ok := t.property(iface, name)
		if !ok {
			c.replyError(m, "org.freedesktop.DBus.Error.UnknownProperty", name+" is not a property here")
			return
		}
		c.reply(m, "v", v)

	case "GetAll":
		iface, _ := args[0].(string)
		c.reply(m, "a{sv}", t.allProperties(iface))

	case "Set":
		// Nothing here is writable; saying so is more use than failing quietly.
		c.replyError(m, "org.freedesktop.DBus.Error.PropertyReadOnly", "these properties are read-only")

	default:
		c.replyError(m, "org.freedesktop.DBus.Error.UnknownMethod", m.Member)
	}
}

func (t *Tray) property(iface, name string) (variant, bool) {
	switch iface {
	case sniIface, "":
		switch name {
		case "Category":
			return vs("SystemServices"), true
		case "Id":
			return vs("myst-relay"), true
		case "Title":
			return vs(t.opts.Title), true
		case "Status":
			return vs("Active"), true
		case "WindowId":
			return vi(0), true
		case "IconName", "OverlayIconName", "AttentionIconName", "AttentionMovieName", "IconThemePath":
			return vs(""), true
		case "IconPixmap":
			return t.pixmaps(), true
		case "OverlayIconPixmap", "AttentionIconPixmap":
			return variant{Sig: "a(iiay)", V: []any{}}, true
		case "ToolTip":
			return t.toolTip(), true
		case "ItemIsMenu":
			// Either button opens the menu, which is how the Windows icon
			// behaves and what makes one click enough on a touchpad.
			return vb(true), true
		case "Menu":
			return vo(menuPath), true
		}
	case menuIface:
		switch name {
		case "Version":
			return variant{Sig: "u", V: uint32(3)}, true
		case "Status":
			return vs("normal"), true
		case "TextDirection":
			return vs("ltr"), true
		case "IconThemePath":
			return vas([]any{}), true
		}
	}
	return variant{}, false
}

func (t *Tray) allProperties(iface string) map[string]variant {
	names := map[string][]string{
		sniIface: {"Category", "Id", "Title", "Status", "WindowId", "IconName", "IconPixmap",
			"OverlayIconName", "OverlayIconPixmap", "AttentionIconName", "AttentionIconPixmap",
			"AttentionMovieName", "ToolTip", "ItemIsMenu", "Menu"},
		menuIface: {"Version", "Status", "TextDirection", "IconThemePath"},
	}[iface]
	out := make(map[string]variant, len(names))
	for _, name := range names {
		if v, ok := t.property(iface, name); ok {
			out[name] = v
		}
	}
	return out
}

// pixmaps is the icon itself, in the several sizes a panel might want.
//
// The wire format is width, height and the pixels as ARGB32 in network byte
// order - so alpha first, and big-endian regardless of the machine. Getting
// that order wrong is the classic way to end up with a blue icon that should
// have been red.
func (t *Tray) pixmaps() variant {
	t.mu.Lock()
	colour := t.colour
	t.mu.Unlock()

	out := make([]any, 0, len(iconSizes))
	for _, size := range iconSizes {
		rgba := discPixels(size, colour)
		argb := make([]byte, len(rgba))
		for i := 0; i < len(rgba); i += 4 {
			argb[i+0] = rgba[i+3]
			argb[i+1] = rgba[i+0]
			argb[i+2] = rgba[i+1]
			argb[i+3] = rgba[i+2]
		}
		out = append(out, []any{int32(size), int32(size), argb})
	}
	return variant{Sig: "a(iiay)", V: out}
}

// toolTip is (icon name, icon pixmaps, title, description).
func (t *Tray) toolTip() variant {
	t.mu.Lock()
	tip := t.tooltip
	t.mu.Unlock()

	title, body, _ := strings.Cut(tip, " - ")
	return variant{
		Sig: "(sa(iiay)ss)",
		V:   []any{"", []any{}, title, body},
	}
}

// menuCall answers com.canonical.dbusmenu.
func (t *Tray) menuCall(c *conn, m *message) {
	args, _ := m.Args()

	switch m.Member {
	case "GetLayout":
		revision, root := t.layout()
		c.reply(m, "u(ia{sv}av)", revision, root)

	case "AboutToShow":
		// The panel is about to draw the menu. Rebuilding here is what makes a
		// menu opened by hand show this moment's figures rather than the last
		// poll's.
		before, _ := t.snapshot()
		t.refreshMenu()
		after, _ := t.snapshot()
		c.reply(m, "b", render(before) != render(after))

	case "AboutToShowGroup":
		t.refreshMenu()
		c.reply(m, "aiai", []any{}, []any{})

	case "Event":
		if len(args) >= 2 {
			id, _ := args[0].(int32)
			if kind, _ := args[1].(string); kind == "clicked" {
				t.activate(id)
			}
		}
		c.reply(m, "")

	case "EventGroup":
		if len(args) >= 1 {
			if events, ok := args[0].([]any); ok {
				for _, e := range events {
					fields, ok := e.([]any)
					if !ok || len(fields) < 2 {
						continue
					}
					id, _ := fields[0].(int32)
					if kind, _ := fields[1].(string); kind == "clicked" {
						t.activate(id)
					}
				}
			}
		}
		c.reply(m, "ai", []any{})

	case "GetGroupProperties":
		items, _ := t.snapshot()
		out := make([]any, 0, len(items))
		for i, item := range items {
			node := menuItem(int32(i+1), item).([]any)
			out = append(out, []any{node[0], node[1]})
		}
		c.reply(m, "a(ia{sv})", out)

	case "GetProperty":
		items, _ := t.snapshot()
		if len(args) >= 2 {
			id, _ := args[0].(int32)
			name, _ := args[1].(string)
			i := int(id) - 1
			if i >= 0 && i < len(items) {
				props := menuItem(id, items[i]).([]any)[1].(map[string]variant)
				if v, ok := props[name]; ok {
					c.reply(m, "v", v)
					return
				}
			}
		}
		c.replyError(m, "org.freedesktop.DBus.Error.InvalidArgs", "no such menu property")

	default:
		c.replyError(m, "org.freedesktop.DBus.Error.UnknownMethod", m.Member)
	}
}

// introspection is the XML a panel may ask for before it trusts the object.
// It lists what is actually implemented and nothing more.
func introspection(path string) string {
	const header = `<!DOCTYPE node PUBLIC "-//freedesktop//DTD D-BUS Object Introspection 1.0//EN" "http://www.freedesktop.org/standards/dbus/1.0/introspect.dtd">
<node>
 <interface name="org.freedesktop.DBus.Introspectable">
  <method name="Introspect"><arg name="xml" type="s" direction="out"/></method>
 </interface>
 <interface name="org.freedesktop.DBus.Properties">
  <method name="Get">
   <arg name="interface" type="s" direction="in"/>
   <arg name="property" type="s" direction="in"/>
   <arg name="value" type="v" direction="out"/>
  </method>
  <method name="GetAll">
   <arg name="interface" type="s" direction="in"/>
   <arg name="properties" type="a{sv}" direction="out"/>
  </method>
 </interface>
`
	switch path {
	case sniPath:
		return header + ` <interface name="org.kde.StatusNotifierItem">
  <property name="Category" type="s" access="read"/>
  <property name="Id" type="s" access="read"/>
  <property name="Title" type="s" access="read"/>
  <property name="Status" type="s" access="read"/>
  <property name="IconName" type="s" access="read"/>
  <property name="IconPixmap" type="a(iiay)" access="read"/>
  <property name="ToolTip" type="(sa(iiay)ss)" access="read"/>
  <property name="ItemIsMenu" type="b" access="read"/>
  <property name="Menu" type="o" access="read"/>
  <method name="Activate"><arg name="x" type="i" direction="in"/><arg name="y" type="i" direction="in"/></method>
  <method name="SecondaryActivate"><arg name="x" type="i" direction="in"/><arg name="y" type="i" direction="in"/></method>
  <method name="ContextMenu"><arg name="x" type="i" direction="in"/><arg name="y" type="i" direction="in"/></method>
  <method name="Scroll"><arg name="delta" type="i" direction="in"/><arg name="dir" type="s" direction="in"/></method>
  <signal name="NewIcon"/>
  <signal name="NewToolTip"/>
  <signal name="NewStatus"><arg name="status" type="s"/></signal>
 </interface>
</node>`
	case menuPath:
		return header + ` <interface name="com.canonical.dbusmenu">
  <property name="Version" type="u" access="read"/>
  <property name="Status" type="s" access="read"/>
  <property name="TextDirection" type="s" access="read"/>
  <property name="IconThemePath" type="as" access="read"/>
  <method name="GetLayout">
   <arg name="parentId" type="i" direction="in"/>
   <arg name="recursionDepth" type="i" direction="in"/>
   <arg name="propertyNames" type="as" direction="in"/>
   <arg name="revision" type="u" direction="out"/>
   <arg name="layout" type="(ia{sv}av)" direction="out"/>
  </method>
  <method name="GetGroupProperties">
   <arg name="ids" type="ai" direction="in"/>
   <arg name="propertyNames" type="as" direction="in"/>
   <arg name="properties" type="a(ia{sv})" direction="out"/>
  </method>
  <method name="GetProperty">
   <arg name="id" type="i" direction="in"/>
   <arg name="name" type="s" direction="in"/>
   <arg name="value" type="v" direction="out"/>
  </method>
  <method name="Event">
   <arg name="id" type="i" direction="in"/>
   <arg name="eventId" type="s" direction="in"/>
   <arg name="data" type="v" direction="in"/>
   <arg name="timestamp" type="u" direction="in"/>
  </method>
  <method name="EventGroup">
   <arg name="events" type="a(isvu)" direction="in"/>
   <arg name="idErrors" type="ai" direction="out"/>
  </method>
  <method name="AboutToShow">
   <arg name="id" type="i" direction="in"/>
   <arg name="needUpdate" type="b" direction="out"/>
  </method>
  <method name="AboutToShowGroup">
   <arg name="ids" type="ai" direction="in"/>
   <arg name="updatesNeeded" type="ai" direction="out"/>
   <arg name="idErrors" type="ai" direction="out"/>
  </method>
  <signal name="LayoutUpdated"><arg name="revision" type="u"/><arg name="parent" type="i"/></signal>
  <signal name="ItemsPropertiesUpdated"><arg name="updated" type="a(ia{sv})"/><arg name="removed" type="a(ias)"/></signal>
 </interface>
</node>`
	}
	return header + "</node>"
}

func machineID() string {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return strconv.Itoa(os.Getpid())
}

// --- text on screen --------------------------------------------------------

// Unlike Windows, the platform here has no message box of its own, so a block
// of text goes to whichever dialog program the desktop happens to have. None of
// them is a dependency: each is tried in turn and the next used if it is
// missing, and a desktop with none of them still gets the text on the terminal
// the tray was started from - the same text the "status", "usage" and "balance"
// commands print, so nothing is unreachable, only less convenient.

// Show puts a report on screen, in a fixed-width font: these are columns of
// figures, and a proportional font turns them into ragged prose.
func Show(title, body string) {
	if !report(title, body) {
		echo(title, body)
	}
}

// Warn says that something went wrong. It is a sentence rather than a report,
// so it goes in the desktop's own warning dialog, icon and all.
func Warn(title, body string) {
	if !alert(title, body) {
		echo(title, body)
	}
}

// report shows text in a scrolling monospace view, fed on standard input.
func report(title, body string) bool {
	for _, tool := range []struct {
		name string
		args []string
	}{
		{"zenity", []string{"--text-info", "--font=Monospace 10", "--width=620", "--height=520", "--title=" + title}},
		{"yad", []string{"--text-info", "--fontname=Monospace 10", "--width=620", "--height=520",
			"--title=" + title, "--button=Close:0"}},
	} {
		if run(tool.name, tool.args, title+"\n\n"+body) {
			return true
		}
	}
	// kdialog reads a file rather than a pipe, and xmessage takes the text as
	// an argument; both use a fixed-width font already.
	if f, err := os.CreateTemp("", "myst-relay-*.txt"); err == nil {
		defer os.Remove(f.Name())
		fmt.Fprintf(f, "%s\n\n%s\n", title, body)
		f.Close()
		if run("kdialog", []string{"--title", title, "--textbox", f.Name(), "640", "520"}, "") {
			return true
		}
	}
	return run("xmessage", []string{"-center", "-title", title, "-buttons", "Close", title + "\n\n" + body}, "")
}

// alert shows one short message with a warning icon.
func alert(title, body string) bool {
	for _, tool := range []struct {
		name string
		args []string
	}{
		{"zenity", []string{"--warning", "--no-markup", "--width=460", "--title=" + title, "--text=" + body}},
		{"yad", []string{"--warning", "--no-markup", "--width=460", "--title=" + title,
			"--text=" + body, "--button=Close:0"}},
		{"kdialog", []string{"--title", title, "--sorry", body}},
		{"xmessage", []string{"-center", "-title", title, "-buttons", "Close", body}},
	} {
		if run(tool.name, tool.args, "") {
			return true
		}
	}
	return false
}

// run shows a dialog and waits for it to be dismissed.
//
// Only a tool that is not installed, or will not start, counts as a failure
// here. Once it is on screen its exit status is the operator's answer to a
// question nobody asked - closing the window with Escape rather than the button
// exits non-zero - and treating that as "the dialog did not work" would print
// the text to a terminal they are not looking at.
func run(tool string, args []string, stdin string) bool {
	path, err := exec.LookPath(tool)
	if err != nil {
		return false
	}
	cmd := exec.Command(path, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	_ = cmd.Wait()
	return true
}

func echo(title, body string) {
	fmt.Printf("\n%s\n%s\n%s\n", title, strings.Repeat("-", len(title)), body)
}

// DetachConsole is the Windows tray's way of getting rid of a console window it
// was handed. Nothing here hands one out, so there is nothing to detach.
func DetachConsole() {}
