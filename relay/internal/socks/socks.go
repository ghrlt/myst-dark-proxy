// Package socks implements the small slice of RFC 1928 / RFC 1929 that the
// relay needs, on both sides of the wire: a client used to dial through the
// myst node's proxy listener, and the wire primitives shared with the SOCKS5
// frontend. It is deliberately dependency-free.
package socks

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Protocol constants.
const (
	Version5 = 0x05

	MethodNoAuth       = 0x00
	MethodUserPass     = 0x02
	MethodNoAcceptable = 0xFF

	AuthVersion = 0x01
	AuthSuccess = 0x00
	AuthFailure = 0x01

	CmdConnect      = 0x01
	CmdBind         = 0x02
	CmdUDPAssociate = 0x03

	AddrIPv4   = 0x01
	AddrDomain = 0x03
	AddrIPv6   = 0x04

	RepSuccess           = 0x00
	RepGeneralFailure    = 0x01
	RepNotAllowed        = 0x02
	RepNetworkUnreach    = 0x03
	RepHostUnreach       = 0x04
	RepRefused           = 0x05
	RepTTLExpired        = 0x06
	RepCommandNotSupport = 0x07
	RepAddrNotSupport    = 0x08
)

// ErrCommandNotSupported is returned when the upstream refuses a command.
var ErrCommandNotSupported = errors.New("socks5: command not supported by upstream")

// Addr is a SOCKS5 target address. Domain names are carried through verbatim so
// name resolution happens at the exit node, never locally: that is what keeps
// the relay free of DNS leaks.
type Addr struct {
	Domain string
	IP     net.IP
	Port   int
}

// ParseAddr splits a host:port string into an Addr.
func ParseAddr(hostport string) (Addr, error) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return Addr{}, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return Addr{}, fmt.Errorf("socks5: bad port %q", portStr)
	}
	a := Addr{Port: port}
	if ip := net.ParseIP(host); ip != nil {
		a.IP = ip
	} else {
		if host == "" || len(host) > 255 {
			return Addr{}, fmt.Errorf("socks5: bad host %q", host)
		}
		a.Domain = host
	}
	return a, nil
}

// String renders the address in host:port form.
func (a Addr) String() string {
	host := a.Domain
	if host == "" && a.IP != nil {
		host = a.IP.String()
	}
	return net.JoinHostPort(host, strconv.Itoa(a.Port))
}

// Marshal appends the ATYP + address + port encoding to dst.
func (a Addr) Marshal(dst []byte) []byte {
	switch {
	case a.Domain != "":
		dst = append(dst, AddrDomain, byte(len(a.Domain)))
		dst = append(dst, a.Domain...)
	case a.IP.To4() != nil:
		dst = append(dst, AddrIPv4)
		dst = append(dst, a.IP.To4()...)
	default:
		dst = append(dst, AddrIPv6)
		dst = append(dst, a.IP.To16()...)
	}
	return binary.BigEndian.AppendUint16(dst, uint16(a.Port))
}

// ReadAddr decodes an ATYP-prefixed address from r.
func ReadAddr(r io.Reader) (Addr, error) {
	var head [1]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Addr{}, err
	}
	var a Addr
	switch head[0] {
	case AddrIPv4:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(r, buf); err != nil {
			return Addr{}, err
		}
		a.IP = net.IP(buf)
	case AddrIPv6:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(r, buf); err != nil {
			return Addr{}, err
		}
		a.IP = net.IP(buf)
	case AddrDomain:
		var l [1]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return Addr{}, err
		}
		buf := make([]byte, l[0])
		if _, err := io.ReadFull(r, buf); err != nil {
			return Addr{}, err
		}
		a.Domain = string(buf)
	default:
		return Addr{}, fmt.Errorf("socks5: unsupported address type 0x%02x", head[0])
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return Addr{}, err
	}
	a.Port = int(binary.BigEndian.Uint16(port[:]))
	return a, nil
}

// Client dials TCP streams through an upstream SOCKS5 proxy.
type Client struct {
	// Address of the upstream proxy, host:port.
	Address string
	// Credentials, optional.
	Username string
	Password string
	// Dial establishes the transport connection to the proxy itself. It must
	// never be a direct-to-internet dialer: the relay reaches the proxy over a
	// container-internal network with no route off-host.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// DialContext opens a tunnelled connection to addr (host:port) via the proxy.
func (c *Client) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if !strings.HasPrefix(network, "tcp") {
		return nil, fmt.Errorf("socks5: unsupported network %q", network)
	}
	target, err := ParseAddr(addr)
	if err != nil {
		return nil, err
	}

	dial := c.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	conn, err := dial(ctx, "tcp", c.Address)
	if err != nil {
		return nil, fmt.Errorf("socks5: reach upstream %s: %w", c.Address, err)
	}

	// Enforce the caller's deadline over the whole handshake, then clear it so
	// the returned connection behaves like a plain net.Conn.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := c.handshake(conn, target); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func (c *Client) handshake(conn net.Conn, target Addr) error {
	methods := []byte{MethodNoAuth}
	if c.Username != "" {
		methods = []byte{MethodUserPass, MethodNoAuth}
	}
	greeting := append([]byte{Version5, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return fmt.Errorf("socks5: greeting: %w", err)
	}

	var sel [2]byte
	if _, err := io.ReadFull(conn, sel[:]); err != nil {
		return fmt.Errorf("socks5: method selection: %w", err)
	}
	if sel[0] != Version5 {
		return fmt.Errorf("socks5: upstream spoke version %d", sel[0])
	}
	switch sel[1] {
	case MethodNoAuth:
	case MethodUserPass:
		if err := c.authenticate(conn); err != nil {
			return err
		}
	default:
		return fmt.Errorf("socks5: upstream rejected our auth methods (0x%02x)", sel[1])
	}

	req := target.Marshal([]byte{Version5, CmdConnect, 0x00})
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: connect request: %w", err)
	}

	var head [3]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fmt.Errorf("socks5: connect reply: %w", err)
	}
	if head[0] != Version5 {
		return fmt.Errorf("socks5: bad reply version %d", head[0])
	}
	if _, err := ReadAddr(conn); err != nil { // BND.ADDR, discarded
		return fmt.Errorf("socks5: connect reply address: %w", err)
	}
	if head[1] != RepSuccess {
		if head[1] == RepCommandNotSupport {
			return ErrCommandNotSupported
		}
		return fmt.Errorf("socks5: upstream refused %s: %s", target, ReplyText(head[1]))
	}
	return nil
}

func (c *Client) authenticate(conn net.Conn) error {
	if len(c.Username) > 255 || len(c.Password) > 255 {
		return errors.New("socks5: credentials too long")
	}
	buf := []byte{AuthVersion, byte(len(c.Username))}
	buf = append(buf, c.Username...)
	buf = append(buf, byte(len(c.Password)))
	buf = append(buf, c.Password...)
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("socks5: auth: %w", err)
	}
	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return fmt.Errorf("socks5: auth reply: %w", err)
	}
	if resp[1] != AuthSuccess {
		return errors.New("socks5: upstream rejected credentials")
	}
	return nil
}

// ReplyText renders a SOCKS5 reply code for logs and error messages.
func ReplyText(code byte) string {
	switch code {
	case RepSuccess:
		return "succeeded"
	case RepGeneralFailure:
		return "general failure"
	case RepNotAllowed:
		return "not allowed by ruleset"
	case RepNetworkUnreach:
		return "network unreachable"
	case RepHostUnreach:
		return "host unreachable"
	case RepRefused:
		return "connection refused"
	case RepTTLExpired:
		return "TTL expired"
	case RepCommandNotSupport:
		return "command not supported"
	case RepAddrNotSupport:
		return "address type not supported"
	default:
		return fmt.Sprintf("unknown reply 0x%02x", code)
	}
}
