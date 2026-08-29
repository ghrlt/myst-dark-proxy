//go:build linux

package systray

import (
	"encoding/binary"
	"fmt"
	"math"
)

// The D-Bus type system, as much of it as a status icon needs.
//
// Values crossing the bus are always a signature plus a Go value that matches
// it. The signature is what drives both directions - there is no reflection
// here and no guessing - so a mismatch is a programming error caught at the
// call, not a malformed message the panel has to make sense of.
//
// The Go shapes are deliberately few:
//
//	basic types   byte, bool, int16/32/64, uint16/32/64, float64, string
//	array   "a…"  []any, or []byte for the "ay" case
//	struct  "(…)" []any, one element per field
//	variant "v"   variant{Sig, V}
//	dict    "a{sv}" map[string]variant, written in a stable order

// variant is a value that carries its own type, which is how D-Bus properties
// and menu item attributes are passed.
type variant struct {
	Sig string
	V   any
}

// vs, vb, vi and vo build the variants this package sends.
func vs(s string) variant  { return variant{"s", s} }
func vb(b bool) variant    { return variant{"b", b} }
func vi(i int32) variant   { return variant{"i", i} }
func vo(p string) variant  { return variant{"o", p} }
func vas(s []any) variant  { return variant{"as", s} }
func vay(b []byte) variant { return variant{"ay", b} }

// --- signatures ------------------------------------------------------------

// sigSplit takes the first complete type off a signature and returns it with
// whatever follows. "a(ii)s" gives "a(ii)" and "s".
func sigSplit(sig string) (one, rest string, err error) {
	if sig == "" {
		return "", "", fmt.Errorf("dbus: empty signature")
	}
	switch sig[0] {
	case 'y', 'b', 'n', 'q', 'i', 'u', 'x', 't', 'd', 's', 'o', 'g', 'v', 'h':
		return sig[:1], sig[1:], nil
	case 'a':
		inner, rest, err := sigSplit(sig[1:])
		if err != nil {
			return "", "", err
		}
		return "a" + inner, rest, nil
	case '(', '{':
		close := byte(')')
		if sig[0] == '{' {
			close = '}'
		}
		depth := 0
		for i := range len(sig) {
			switch sig[i] {
			case '(', '{':
				depth++
			case ')', '}':
				depth--
				if depth == 0 {
					if sig[i] != close {
						return "", "", fmt.Errorf("dbus: mismatched brackets in %q", sig)
					}
					return sig[:i+1], sig[i+1:], nil
				}
			}
		}
		return "", "", fmt.Errorf("dbus: unterminated %q", sig)
	}
	return "", "", fmt.Errorf("dbus: unknown type code %q", sig[0])
}

// sigAll splits a whole signature into its top-level types.
func sigAll(sig string) ([]string, error) {
	var out []string
	for sig != "" {
		one, rest, err := sigSplit(sig)
		if err != nil {
			return nil, err
		}
		out, sig = append(out, one), rest
	}
	return out, nil
}

// alignOf is the boundary a value of this type starts on.
func alignOf(sig string) int {
	switch sig[0] {
	case 'y', 'g', 'v':
		return 1
	case 'n', 'q':
		return 2
	case 'b', 'i', 'u', 's', 'o', 'a', 'h':
		return 4
	case 'x', 't', 'd', '(', '{':
		return 8
	}
	return 1
}

// --- encoding --------------------------------------------------------------

type enc struct{ b []byte }

// pad advances to the next boundary. D-Bus pads with zeroes, and the padding
// is part of the message rather than something the reader may skip freely, so
// every write goes through here.
func (e *enc) pad(to int) {
	for len(e.b)%to != 0 {
		e.b = append(e.b, 0)
	}
}

func (e *enc) u32(v uint32) {
	e.pad(4)
	e.b = binary.LittleEndian.AppendUint32(e.b, v)
}

func (e *enc) str(s string) {
	e.u32(uint32(len(s)))
	e.b = append(e.b, s...)
	e.b = append(e.b, 0)
}

func (e *enc) signature(s string) {
	e.b = append(e.b, byte(len(s)))
	e.b = append(e.b, s...)
	e.b = append(e.b, 0)
}

// value writes v as the type named by sig.
func (e *enc) value(sig string, v any) error {
	switch sig[0] {
	case 'y':
		b, ok := v.(byte)
		if !ok {
			return typeErr(sig, v)
		}
		e.b = append(e.b, b)
	case 'b':
		b, ok := v.(bool)
		if !ok {
			return typeErr(sig, v)
		}
		var n uint32
		if b {
			n = 1
		}
		e.u32(n)
	case 'n', 'q':
		var n uint16
		switch t := v.(type) {
		case int16:
			n = uint16(t)
		case uint16:
			n = t
		default:
			return typeErr(sig, v)
		}
		e.pad(2)
		e.b = binary.LittleEndian.AppendUint16(e.b, n)
	case 'i', 'u', 'h':
		var n uint32
		switch t := v.(type) {
		case int32:
			n = uint32(t)
		case uint32:
			n = t
		case int:
			n = uint32(t)
		default:
			return typeErr(sig, v)
		}
		e.u32(n)
	case 'x', 't':
		var n uint64
		switch t := v.(type) {
		case int64:
			n = uint64(t)
		case uint64:
			n = t
		default:
			return typeErr(sig, v)
		}
		e.pad(8)
		e.b = binary.LittleEndian.AppendUint64(e.b, n)
	case 'd':
		f, ok := v.(float64)
		if !ok {
			return typeErr(sig, v)
		}
		e.pad(8)
		e.b = binary.LittleEndian.AppendUint64(e.b, math.Float64bits(f))
	case 's', 'o':
		s, ok := v.(string)
		if !ok {
			return typeErr(sig, v)
		}
		e.str(s)
	case 'g':
		s, ok := v.(string)
		if !ok {
			return typeErr(sig, v)
		}
		e.signature(s)
	case 'v':
		val, ok := v.(variant)
		if !ok {
			return typeErr(sig, v)
		}
		e.signature(val.Sig)
		return e.value(val.Sig, val.V)
	case 'a':
		return e.array(sig[1:], v)
	case '(':
		fields, err := sigAll(sig[1 : len(sig)-1])
		if err != nil {
			return err
		}
		items, ok := v.([]any)
		if !ok || len(items) != len(fields) {
			return typeErr(sig, v)
		}
		e.pad(8)
		for i, f := range fields {
			if err := e.value(f, items[i]); err != nil {
				return err
			}
		}
	case '{':
		return fmt.Errorf("dbus: a dict entry is only written inside an array")
	default:
		return fmt.Errorf("dbus: cannot write type %q", sig)
	}
	return nil
}

// array writes the elements of an array whose element type is elemSig.
//
// The length prefix counts the bytes of the elements alone, so it can only be
// filled in once they are written: it goes down as a placeholder, and the
// padding that follows it is deliberately excluded from the count.
func (e *enc) array(elemSig string, v any) error {
	e.u32(0)
	lenAt := len(e.b) - 4
	e.pad(alignOf(elemSig))
	start := len(e.b)

	switch {
	case elemSig == "y":
		b, ok := v.([]byte)
		if !ok {
			return typeErr("ay", v)
		}
		e.b = append(e.b, b...)

	case elemSig[0] == '{':
		// A dict. Only a{sv} is needed here, and writing it in sorted order
		// keeps a message that says the same thing byte-identical, which is
		// what lets the menu be compared rather than resent.
		if elemSig != "{sv}" {
			return fmt.Errorf("dbus: only a{sv} dictionaries are supported, not a%s", elemSig)
		}
		m, ok := v.(map[string]variant)
		if !ok {
			return typeErr("a"+elemSig, v)
		}
		for _, k := range sortedKeys(m) {
			e.pad(8)
			e.str(k)
			if err := e.value("v", m[k]); err != nil {
				return err
			}
		}

	default:
		items, ok := v.([]any)
		if !ok {
			return typeErr("a"+elemSig, v)
		}
		for _, item := range items {
			if err := e.value(elemSig, item); err != nil {
				return err
			}
		}
	}

	binary.LittleEndian.PutUint32(e.b[lenAt:], uint32(len(e.b)-start))
	return nil
}

func typeErr(sig string, v any) error {
	return fmt.Errorf("dbus: %T is not a %s", v, sig)
}

func sortedKeys(m map[string]variant) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// marshal writes a whole argument list.
func marshal(sig string, args ...any) ([]byte, error) {
	types, err := sigAll(sig)
	if err != nil {
		return nil, err
	}
	if len(types) != len(args) {
		return nil, fmt.Errorf("dbus: signature %q wants %d arguments, got %d", sig, len(types), len(args))
	}
	var e enc
	for i, t := range types {
		if err := e.value(t, args[i]); err != nil {
			return nil, err
		}
	}
	return e.b, nil
}

// --- decoding --------------------------------------------------------------

type dec struct {
	b   []byte
	i   int
	end bool
}

func (d *dec) pad(to int) error {
	for d.i%to != 0 {
		if d.i >= len(d.b) {
			return fmt.Errorf("dbus: truncated message")
		}
		d.i++
	}
	return nil
}

func (d *dec) take(n int) ([]byte, error) {
	if d.i+n > len(d.b) {
		return nil, fmt.Errorf("dbus: truncated message")
	}
	out := d.b[d.i : d.i+n]
	d.i += n
	return out, nil
}

func (d *dec) order() binary.ByteOrder {
	if d.end {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

func (d *dec) u32() (uint32, error) {
	if err := d.pad(4); err != nil {
		return 0, err
	}
	b, err := d.take(4)
	if err != nil {
		return 0, err
	}
	return d.order().Uint32(b), nil
}

func (d *dec) str() (string, error) {
	n, err := d.u32()
	if err != nil {
		return "", err
	}
	b, err := d.take(int(n) + 1) // the trailing NUL is not part of the string
	if err != nil {
		return "", err
	}
	return string(b[:n]), nil
}

func (d *dec) signature() (string, error) {
	b, err := d.take(1)
	if err != nil {
		return "", err
	}
	s, err := d.take(int(b[0]) + 1)
	if err != nil {
		return "", err
	}
	return string(s[:len(s)-1]), nil
}

// value reads one value of the type named by sig.
func (d *dec) value(sig string) (any, error) {
	switch sig[0] {
	case 'y':
		b, err := d.take(1)
		if err != nil {
			return nil, err
		}
		return b[0], nil
	case 'b':
		n, err := d.u32()
		return n != 0, err
	case 'n', 'q':
		if err := d.pad(2); err != nil {
			return nil, err
		}
		b, err := d.take(2)
		if err != nil {
			return nil, err
		}
		v := d.order().Uint16(b)
		if sig[0] == 'n' {
			return int16(v), nil
		}
		return v, nil
	case 'i', 'u', 'h':
		v, err := d.u32()
		if err != nil {
			return nil, err
		}
		if sig[0] == 'i' {
			return int32(v), nil
		}
		return v, nil
	case 'x', 't':
		if err := d.pad(8); err != nil {
			return nil, err
		}
		b, err := d.take(8)
		if err != nil {
			return nil, err
		}
		v := d.order().Uint64(b)
		if sig[0] == 'x' {
			return int64(v), nil
		}
		return v, nil
	case 'd':
		if err := d.pad(8); err != nil {
			return nil, err
		}
		b, err := d.take(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(d.order().Uint64(b)), nil
	case 's', 'o':
		return d.str()
	case 'g':
		return d.signature()
	case 'v':
		inner, err := d.signature()
		if err != nil {
			return nil, err
		}
		v, err := d.value(inner)
		if err != nil {
			return nil, err
		}
		return variant{Sig: inner, V: v}, nil
	case 'a':
		return d.array(sig[1:])
	case '(':
		fields, err := sigAll(sig[1 : len(sig)-1])
		if err != nil {
			return nil, err
		}
		if err := d.pad(8); err != nil {
			return nil, err
		}
		out := make([]any, 0, len(fields))
		for _, f := range fields {
			v, err := d.value(f)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case '{':
		fields, err := sigAll(sig[1 : len(sig)-1])
		if err != nil || len(fields) != 2 {
			return nil, fmt.Errorf("dbus: bad dict entry %q", sig)
		}
		if err := d.pad(8); err != nil {
			return nil, err
		}
		k, err := d.value(fields[0])
		if err != nil {
			return nil, err
		}
		v, err := d.value(fields[1])
		if err != nil {
			return nil, err
		}
		return []any{k, v}, nil
	}
	return nil, fmt.Errorf("dbus: cannot read type %q", sig)
}

func (d *dec) array(elemSig string) (any, error) {
	n, err := d.u32()
	if err != nil {
		return nil, err
	}
	if err := d.pad(alignOf(elemSig)); err != nil {
		return nil, err
	}
	stop := d.i + int(n)
	if stop > len(d.b) {
		return nil, fmt.Errorf("dbus: array runs past the end of the message")
	}
	if elemSig == "y" {
		b, err := d.take(int(n))
		if err != nil {
			return nil, err
		}
		return append([]byte(nil), b...), nil
	}
	var out []any
	for d.i < stop {
		v, err := d.value(elemSig)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	d.i = stop
	return out, nil
}

// unmarshal reads a whole argument list.
func unmarshal(sig string, body []byte, bigEndian bool) ([]any, error) {
	types, err := sigAll(sig)
	if err != nil {
		return nil, err
	}
	d := &dec{b: body, end: bigEndian}
	out := make([]any, 0, len(types))
	for _, t := range types {
		v, err := d.value(t)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
