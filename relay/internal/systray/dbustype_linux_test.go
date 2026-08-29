//go:build linux

package systray

import (
	"bufio"
	"bytes"
	"reflect"
	"testing"
)

// bufReader is what readMessage reads from.
func bufReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }

// The marshalling is the part of the Linux tray that a panel judges silently:
// a message off by one byte is not an error, it is an icon that never appears
// or a menu that comes out blank. So the round trip is checked here for every
// shape the StatusNotifierItem and dbusmenu protocols actually send.

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		sig  string
		args []any
	}{
		{"a string", "s", []any{"myst-relay"}},
		{"the empties", "sas", []any{"", []any(nil)}},
		{"numbers of every width", "ynqiuxtd", []any{
			byte(7), int16(-2), uint16(9), int32(-42), uint32(42), int64(-1 << 40), uint64(1 << 40), 1.5,
		}},
		{"a path and a signature", "og", []any{"/StatusNotifierItem", "a(iiay)"}},
		{"a variant", "v", []any{vs("Active")}},
		{"the header fields", "a(yv)", []any{[]any{
			[]any{byte(fieldPath), vo("/MenuBar")},
			[]any{byte(fieldMember), vs("GetLayout")},
			[]any{byte(fieldReplySerial), variant{Sig: "u", V: uint32(3)}},
		}}},
		{"an icon", "a(iiay)", []any{[]any{
			[]any{int32(2), int32(1), []byte{1, 2, 3, 4, 5, 6, 7, 8}},
			[]any{int32(1), int32(1), []byte{9, 8, 7, 6}},
		}}},
		{"a tooltip", "(sa(iiay)ss)", []any{
			[]any{"", []any{}, "myst-relay", "carrying traffic"},
		}},
		{"a menu layout", "u(ia{sv}av)", []any{
			uint32(4),
			[]any{
				int32(0),
				map[string]variant{"children-display": vs("submenu")},
				[]any{
					variant{Sig: "(ia{sv}av)", V: []any{
						int32(1),
						map[string]variant{"label": vs("Status..."), "enabled": vb(true)},
						[]any{},
					}},
					variant{Sig: "(ia{sv}av)", V: []any{
						int32(2),
						map[string]variant{"type": vs("separator")},
						[]any{},
					}},
				},
			},
		}},
		// A byte array of an awkward length, to catch padding that only works
		// when the numbers happen to divide.
		{"an odd array", "ays", []any{[]byte{1, 2, 3}, "after"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := marshal(tc.sig, tc.args...)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := unmarshal(tc.sig, body, false)
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, normalise(tc.args)) {
				t.Errorf("round trip changed the value\n got %#v\nwant %#v", got, tc.args)
			}
		})
	}
}

// normalise expresses the arguments the way a decoder must return them: an
// empty array reads back as no elements whether it was written as nil or as an
// empty slice, and a dictionary reads back as the pairs it was written from.
func normalise(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = normaliseOne(a)
	}
	return out
}

func normaliseOne(v any) any {
	switch t := v.(type) {
	case []any:
		if len(t) == 0 {
			return []any(nil)
		}
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normaliseOne(e)
		}
		return out
	case map[string]variant:
		out := make([]any, 0, len(t))
		for _, k := range sortedKeys(t) {
			out = append(out, []any{k, normaliseOne(t[k])})
		}
		if len(out) == 0 {
			return []any(nil)
		}
		return out
	case variant:
		return variant{Sig: t.Sig, V: normaliseOne(t.V)}
	}
	return v
}

// A message must survive being framed and read back, because that framing is
// where the alignment rules bite: the body starts on an eight-byte boundary
// measured from the front of the message, not from the end of the header.
func TestFrameAndReadBack(t *testing.T) {
	// Header lengths vary with the member name, and each length exercises a
	// different amount of padding before the body.
	for _, member := range []string{"A", "Hello", "GetLayout", "RegisterStatusNotifierItem"} {
		body, err := marshal("su", "myst-relay", uint32(1))
		if err != nil {
			t.Fatal(err)
		}
		out := &message{
			Type:        msgCall,
			Serial:      11,
			Path:        "/StatusNotifierItem",
			Interface:   "org.kde.StatusNotifierItem",
			Member:      member,
			Destination: "org.kde.StatusNotifierWatcher",
			Signature:   "su",
			Body:        body,
		}
		raw, err := frame(out)
		if err != nil {
			t.Fatalf("%s: frame: %v", member, err)
		}
		in, err := readMessage(bufReader(raw))
		if err != nil {
			t.Fatalf("%s: read back: %v", member, err)
		}
		if in.Member != member || in.Path != out.Path || in.Signature != out.Signature {
			t.Fatalf("%s: header came back wrong: %+v", member, in)
		}
		args, err := in.Args()
		if err != nil {
			t.Fatalf("%s: body: %v", member, err)
		}
		if len(args) != 2 || args[0] != "myst-relay" || args[1] != uint32(1) {
			t.Errorf("%s: body came back as %#v", member, args)
		}
	}
}

func TestSignatureSplitting(t *testing.T) {
	cases := map[string][]string{
		"":             nil,
		"s":            {"s"},
		"su":           {"s", "u"},
		"a(iiay)":      {"a(iiay)"},
		"u(ia{sv}av)":  {"u", "(ia{sv}av)"},
		"a{sv}i":       {"a{sv}", "i"},
		"(sa(iiay)ss)": {"(sa(iiay)ss)"},
	}
	for sig, want := range cases {
		got, err := sigAll(sig)
		if err != nil {
			t.Errorf("%q: %v", sig, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q split into %v, want %v", sig, got, want)
		}
	}
	for _, bad := range []string{"(", "a", "(is", "z"} {
		if _, err := sigAll(bad); err == nil {
			t.Errorf("%q should not have parsed", bad)
		}
	}
}

// The icon has to reach the panel as ARGB32, and every pixel of it: a size
// whose byte count is wrong is an icon the panel silently declines to draw.
func TestIconIsWholeAndOpaqueInTheMiddle(t *testing.T) {
	for _, size := range iconSizes {
		px := discPixels(size, Green)
		if len(px) != size*size*4 {
			t.Fatalf("%dx%d came to %d bytes, want %d", size, size, len(px), size*size*4)
		}
		centre := ((size/2)*size + size/2) * 4
		if px[centre+3] != 255 {
			t.Errorf("%dx%d: the middle of the disc is not opaque (alpha %d)", size, size, px[centre+3])
		}
		if px[centre] != Green.R || px[centre+1] != Green.G || px[centre+2] != Green.B {
			t.Errorf("%dx%d: the middle is %d,%d,%d, want the fill colour",
				size, size, px[centre], px[centre+1], px[centre+2])
		}
		if px[3] != 0 {
			t.Errorf("%dx%d: the corner is not transparent (alpha %d)", size, size, px[3])
		}
	}
}
