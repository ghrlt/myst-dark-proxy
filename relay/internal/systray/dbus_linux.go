//go:build linux

package systray

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A small D-Bus client: enough to own a name, export two objects, answer the
// property reads a panel makes of them, emit a few signals, and place a handful
// of calls.
//
// The alternative was a dependency, and this repository has none: a relay whose
// whole promise is "nothing leaves except through the exit" is a poor place to
// start taking code nobody here has read. What that costs is the marshalling in
// dbustype_linux.go and the framing below.

const (
	msgCall   = 1
	msgReturn = 2
	msgError  = 3
	msgSignal = 4

	// noReplyExpected lets a signal or a fire-and-forget call go out without
	// the bus manufacturing a reply nobody is waiting for.
	noReplyExpected = 0x1

	fieldPath        = 1
	fieldInterface   = 2
	fieldMember      = 3
	fieldErrorName   = 4
	fieldReplySerial = 5
	fieldDestination = 6
	fieldSender      = 7
	fieldSignature   = 8
)

// message is one D-Bus message, with its arguments still in wire form. Bodies
// are decoded at the point of use, where the expected signature is known.
type message struct {
	Type        byte
	Flags       byte
	Serial      uint32
	Path        string
	Interface   string
	Member      string
	ErrorName   string
	Destination string
	Sender      string
	Signature   string
	ReplySerial uint32
	Body        []byte
	BigEndian   bool
}

// Args decodes the body against the signature the sender declared.
func (m *message) Args() ([]any, error) {
	if m.Signature == "" {
		return nil, nil
	}
	return unmarshal(m.Signature, m.Body, m.BigEndian)
}

// conn is a connection to a message bus.
type conn struct {
	c      net.Conn
	r      *bufio.Reader
	unique string

	wmu    sync.Mutex // one writer at a time, and the serial counter with it
	serial uint32

	pmu     sync.Mutex
	pending map[uint32]chan *message

	// onCall handles incoming method calls. It runs on the read goroutine, so
	// it must answer promptly and hand slow work to a goroutine of its own.
	onCall func(*conn, *message)

	closeOnce sync.Once
	closed    chan struct{}
}

// dial connects to the session bus and completes the handshake.
func dial() (*conn, error) {
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if addr == "" {
		// The usual place systemd puts it, for a process started without the
		// session's environment.
		addr = "unix:path=/run/user/" + strconv.Itoa(os.Getuid()) + "/bus"
	}
	network, address, err := parseAddress(addr)
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout(network, address, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to the session bus: %w", err)
	}
	conn := &conn{
		c:       c,
		r:       bufio.NewReader(c),
		pending: map[uint32]chan *message{},
		closed:  make(chan struct{}),
	}
	if err := conn.authenticate(); err != nil {
		c.Close()
		return nil, err
	}
	go conn.read()

	unique, err := conn.callString("org.freedesktop.DBus", "/org/freedesktop/DBus",
		"org.freedesktop.DBus", "Hello")
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("say hello to the bus: %w", err)
	}
	conn.unique = unique
	return conn, nil
}

// parseAddress understands the two forms a session bus address takes. Several
// may be offered, separated by semicolons; the first usable one wins.
func parseAddress(addr string) (network, address string, err error) {
	for _, candidate := range strings.Split(addr, ";") {
		kind, args, ok := strings.Cut(candidate, ":")
		if !ok || kind != "unix" {
			continue
		}
		for _, pair := range strings.Split(args, ",") {
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			switch k {
			case "path":
				return "unix", v, nil
			case "abstract":
				// Linux abstract sockets are named with a leading NUL.
				return "unix", "@" + v, nil
			}
		}
	}
	return "", "", fmt.Errorf("no usable unix socket in DBUS_SESSION_BUS_ADDRESS=%q", addr)
}

// authenticate performs the SASL exchange the bus expects before any message.
//
// EXTERNAL is the mechanism a session bus uses: the kernel already told it who
// is on the other end of the socket, so the "credential" is just this process
// saying which uid it believes itself to be.
func (c *conn) authenticate() error {
	if _, err := c.c.Write([]byte{0}); err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	if err := c.line("AUTH EXTERNAL " + hexOf(uid)); err != nil {
		return err
	}
	reply, err := c.r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("bus authentication: %w", err)
	}
	if !strings.HasPrefix(reply, "OK ") {
		return fmt.Errorf("bus refused authentication: %s", strings.TrimSpace(reply))
	}
	return c.line("BEGIN")
}

func (c *conn) line(s string) error {
	_, err := c.c.Write([]byte(s + "\r\n"))
	return err
}

func hexOf(s string) string {
	const digits = "0123456789abcdef"
	var b strings.Builder
	for i := range len(s) {
		b.WriteByte(digits[s[i]>>4])
		b.WriteByte(digits[s[i]&0xF])
	}
	return b.String()
}

func (c *conn) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.c.Close()
	})
}

// Done is closed when the connection drops.
func (c *conn) Done() <-chan struct{} { return c.closed }

// --- writing ---------------------------------------------------------------

// send writes one message, allocating its serial. A zero reply channel means
// nothing is waiting for an answer.
func (c *conn) send(m *message, wait chan *message) (uint32, error) {
	c.wmu.Lock()
	c.serial++
	serial := c.serial
	if wait != nil {
		c.pmu.Lock()
		c.pending[serial] = wait
		c.pmu.Unlock()
	}
	m.Serial = serial

	buf, err := frame(m)
	if err == nil {
		_, err = c.c.Write(buf)
	}
	c.wmu.Unlock()

	if err != nil && wait != nil {
		c.pmu.Lock()
		delete(c.pending, serial)
		c.pmu.Unlock()
	}
	return serial, err
}

// frame renders a message onto the wire.
func frame(m *message) ([]byte, error) {
	var fields []any
	add := func(code byte, sig string, v any) {
		fields = append(fields, []any{code, variant{Sig: sig, V: v}})
	}
	if m.Path != "" {
		add(fieldPath, "o", m.Path)
	}
	if m.Interface != "" {
		add(fieldInterface, "s", m.Interface)
	}
	if m.Member != "" {
		add(fieldMember, "s", m.Member)
	}
	if m.ErrorName != "" {
		add(fieldErrorName, "s", m.ErrorName)
	}
	if m.ReplySerial != 0 {
		add(fieldReplySerial, "u", m.ReplySerial)
	}
	if m.Destination != "" {
		add(fieldDestination, "s", m.Destination)
	}
	if m.Signature != "" {
		add(fieldSignature, "g", m.Signature)
	}

	var e enc
	e.b = append(e.b, 'l', m.Type, m.Flags, 1)
	e.b = binary.LittleEndian.AppendUint32(e.b, uint32(len(m.Body)))
	e.b = binary.LittleEndian.AppendUint32(e.b, m.Serial)
	if err := e.value("a(yv)", fields); err != nil {
		return nil, err
	}
	// The body always starts on an eight-byte boundary, whatever the header
	// happened to end on.
	e.pad(8)
	return append(e.b, m.Body...), nil
}

// --- reading ---------------------------------------------------------------

func (c *conn) read() {
	defer c.Close()
	for {
		m, err := readMessage(c.r)
		if err != nil {
			return
		}
		switch m.Type {
		case msgReturn, msgError:
			c.pmu.Lock()
			ch := c.pending[m.ReplySerial]
			delete(c.pending, m.ReplySerial)
			c.pmu.Unlock()
			if ch != nil {
				ch <- m
			}
		case msgCall:
			if c.onCall != nil {
				c.onCall(c, m)
			} else {
				c.replyError(m, "org.freedesktop.DBus.Error.UnknownObject", "nothing is exported here")
			}
		}
	}
}

func readMessage(r *bufio.Reader) (*message, error) {
	head := make([]byte, 16)
	if _, err := ioReadFull(r, head); err != nil {
		return nil, err
	}
	big := head[0] == 'B'
	order := binary.ByteOrder(binary.LittleEndian)
	if big {
		order = binary.BigEndian
	}
	m := &message{Type: head[1], Flags: head[2], BigEndian: big}
	bodyLen := order.Uint32(head[4:8])
	m.Serial = order.Uint32(head[8:12])
	fieldsLen := order.Uint32(head[12:16])

	// The header fields, then the padding that puts the body on an eight-byte
	// boundary. The sixteen bytes already read include the field array's own
	// length prefix and are themselves a multiple of eight, so what has to be
	// rounded up is the length of the fields alone.
	padded := int(fieldsLen)
	for padded%8 != 0 {
		padded++
	}
	rest := make([]byte, padded+int(bodyLen))
	if _, err := ioReadFull(r, rest); err != nil {
		return nil, err
	}

	d := &dec{b: rest[:fieldsLen], end: big}
	for d.i < len(d.b) {
		v, err := d.value("(yv)")
		if err != nil {
			return nil, err
		}
		pair := v.([]any)
		code, _ := pair[0].(byte)
		val, _ := pair[1].(variant)
		s, _ := val.V.(string)
		switch code {
		case fieldPath:
			m.Path = s
		case fieldInterface:
			m.Interface = s
		case fieldMember:
			m.Member = s
		case fieldErrorName:
			m.ErrorName = s
		case fieldReplySerial:
			m.ReplySerial, _ = val.V.(uint32)
		case fieldDestination:
			m.Destination = s
		case fieldSender:
			m.Sender = s
		case fieldSignature:
			m.Signature = s
		}
	}
	m.Body = rest[padded:]
	return m, nil
}

// ioReadFull is io.ReadFull, spelled out to keep the import list to what this
// file otherwise needs.
func ioReadFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		got, err := r.Read(buf[n:])
		n += got
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// --- calls -----------------------------------------------------------------

// busError is what the other end said went wrong.
type busError struct {
	Name string
	Text string
}

func (e *busError) Error() string {
	if e.Text != "" {
		return e.Name + ": " + e.Text
	}
	return e.Name
}

// call places a method call and waits for the answer.
func (c *conn) call(dest, path, iface, member, sig string, args ...any) (*message, error) {
	body, err := marshal(sig, args...)
	if err != nil {
		return nil, err
	}
	wait := make(chan *message, 1)
	if _, err := c.send(&message{
		Type:        msgCall,
		Destination: dest,
		Path:        path,
		Interface:   iface,
		Member:      member,
		Signature:   sig,
		Body:        body,
	}, wait); err != nil {
		return nil, err
	}
	select {
	case reply := <-wait:
		if reply.Type == msgError {
			text := ""
			if args, err := reply.Args(); err == nil && len(args) > 0 {
				text, _ = args[0].(string)
			}
			return nil, &busError{Name: reply.ErrorName, Text: text}
		}
		return reply, nil
	case <-c.closed:
		return nil, fmt.Errorf("dbus: the connection closed while waiting for %s", member)
	case <-time.After(25 * time.Second):
		return nil, fmt.Errorf("dbus: %s did not answer", member)
	}
}

// callString is a call whose reply is a single string.
func (c *conn) callString(dest, path, iface, member string) (string, error) {
	reply, err := c.call(dest, path, iface, member, "")
	if err != nil {
		return "", err
	}
	args, err := reply.Args()
	if err != nil || len(args) == 0 {
		return "", fmt.Errorf("dbus: %s answered nothing", member)
	}
	s, _ := args[0].(string)
	return s, nil
}

// emit broadcasts a signal.
func (c *conn) emit(path, iface, member, sig string, args ...any) error {
	body, err := marshal(sig, args...)
	if err != nil {
		return err
	}
	_, err = c.send(&message{
		Type:      msgSignal,
		Flags:     noReplyExpected,
		Path:      path,
		Interface: iface,
		Member:    member,
		Signature: sig,
		Body:      body,
	}, nil)
	return err
}

// reply answers a method call.
func (c *conn) reply(to *message, sig string, args ...any) {
	if to.Flags&noReplyExpected != 0 {
		return
	}
	body, err := marshal(sig, args...)
	if err != nil {
		c.replyError(to, "org.freedesktop.DBus.Error.Failed", err.Error())
		return
	}
	_, _ = c.send(&message{
		Type:        msgReturn,
		Flags:       noReplyExpected,
		Destination: to.Sender,
		ReplySerial: to.Serial,
		Signature:   sig,
		Body:        body,
	}, nil)
}

func (c *conn) replyError(to *message, name, text string) {
	if to.Flags&noReplyExpected != 0 {
		return
	}
	body, _ := marshal("s", text)
	_, _ = c.send(&message{
		Type:        msgError,
		Flags:       noReplyExpected,
		Destination: to.Sender,
		ReplySerial: to.Serial,
		ErrorName:   name,
		Signature:   "s",
		Body:        body,
	}, nil)
}

// requestName claims a well-known name. Owning one is what makes this process
// findable: the watcher is handed the name, and asks it for the icon.
func (c *conn) requestName(name string) error {
	const doNotQueue = 0x4
	reply, err := c.call("org.freedesktop.DBus", "/org/freedesktop/DBus",
		"org.freedesktop.DBus", "RequestName", "su", name, uint32(doNotQueue))
	if err != nil {
		return err
	}
	args, err := reply.Args()
	if err != nil || len(args) == 0 {
		return fmt.Errorf("dbus: RequestName answered nothing")
	}
	// 1 is primary owner, 4 is "already the owner".
	if code, _ := args[0].(uint32); code != 1 && code != 4 {
		return fmt.Errorf("dbus: could not take the name %s (code %d)", name, code)
	}
	return nil
}
