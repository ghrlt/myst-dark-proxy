// Package systray puts an icon in the desktop's notification area.
//
// Two desktops are covered, and each is spoken to in its own protocol rather
// than through a portability layer: Win32 and Shell_NotifyIcon on Windows, and
// StatusNotifierItem over D-Bus on Linux, which is what a modern panel - XFCE,
// KDE, GNOME with an extension - actually listens for. The vocabulary above
// them is the same, so the code that decides what the menu says is written once.
//
// It is written against those two directly rather than against a tray library,
// because this repository has no third-party dependencies and a relay whose
// whole promise is "nothing leaves except through the exit" is a poor place to
// start acquiring some. What that costs is the declarations and the D-Bus
// marshalling; what it buys is that the only code in the binary is code that
// was read before it went in.
//
// This file is the part both share: the vocabulary, and the drawing of the icon
// itself.
package systray

// Colour is the fill of the notification-area icon, which is drawn rather than
// shipped: one small disc in a colour that says what the relay is doing.
type Colour struct{ R, G, B uint8 }

// The states an icon is drawn in. They are deliberately far apart in hue as
// well as in brightness, because a notification area is small and often looked
// at out of the corner of an eye.
var (
	Green = Colour{0x2E, 0xA0, 0x43} // carrying traffic
	Blue  = Colour{0x37, 0x7E, 0xC8} // resting: no session, nothing wrong
	Amber = Colour{0xD2, 0x91, 0x22} // building a session
	Red   = Colour{0xC0, 0x39, 0x2B} // refusing: the tunnel is down
	Grey  = Colour{0x80, 0x86, 0x8B} // no relay is answering
)

// Item is one line of the menu.
//
// The menu is rebuilt every time it is opened, so an item may carry a live
// value in its label and a disabled item is a perfectly good way to show one.
type Item struct {
	Label     string
	Separator bool
	Disabled  bool
	// Bold marks the default item: the one a double-click on the icon runs.
	Bold bool
	// Do is run on a goroutine of its own, so an action that opens a dialog or
	// waits on the network does not freeze the icon it was started from.
	Do func()
}

// Separator is the divider between groups of menu items.
func Separator() Item { return Item{Separator: true} }

// Label is a menu line that shows something rather than doing something.
func Label(text string) Item { return Item{Label: text, Disabled: true} }

// Options describe the icon to put up.
type Options struct {
	// Title names the icon to the desktop. It is not usually shown.
	Title string
	// Tooltip is the hover text, and Colour the initial fill.
	Tooltip string
	Colour  Colour
	// Menu is called each time the menu is opened, so the menu always shows
	// the present state rather than the state when the icon went up.
	Menu func() []Item
	// Ready is called once the icon is up, with the handle for changing it.
	Ready func(*Tray)
}

// --- drawing ---------------------------------------------------------------

// iconSizes are the sizes an icon is offered in where the desktop chooses one
// rather than being told. Panels vary from sixteen pixels to a scaled-up
// forty-eight, and drawing each is cheaper than scaling one badly.
var iconSizes = []int{16, 22, 24, 32, 48}

// discPixels draws the icon: a filled disc with a darker rim, in straight
// (non-premultiplied) RGBA, four samples to a pixel on each axis so the edge is
// smooth even at sixteen pixels across.
//
// Drawing it rather than shipping a set of image files is what lets the icon
// carry the relay's state in its colour without the binary carrying five
// images, and what makes it come out crisp at whatever size the desktop asks
// for. Callers reorder the channels into whatever their own platform wants.
func discPixels(size int, c Colour) []byte {
	const samples = 4

	out := make([]byte, size*size*4)
	centre := float64(size) / 2
	outer := centre - 0.6
	inner := outer - max(1.1, float64(size)/12)
	rim := Colour{R: dim(c.R), G: dim(c.G), B: dim(c.B)}

	for y := range size {
		for x := range size {
			var covered, rimmed int
			for sy := range samples {
				for sx := range samples {
					px := float64(x) + (float64(sx)+0.5)/samples - centre
					py := float64(y) + (float64(sy)+0.5)/samples - centre
					d := px*px + py*py
					if d > outer*outer {
						continue
					}
					covered++
					if d > inner*inner {
						rimmed++
					}
				}
			}
			if covered == 0 {
				continue
			}
			fill := c
			if rimmed*2 > covered {
				fill = rim
			}
			i := (y*size + x) * 4
			out[i+0] = fill.R
			out[i+1] = fill.G
			out[i+2] = fill.B
			out[i+3] = byte(covered * 255 / (samples * samples))
		}
	}
	return out
}

func dim(v uint8) uint8 { return uint8(int(v) * 62 / 100) }
