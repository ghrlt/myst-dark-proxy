package socks5

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/frontend"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/gate"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/socks"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tunnel"
)

// fakeTunnel stands in for the myst session. It counts dials so a test can
// assert that a refused request never reached the egress path at all.
type fakeTunnel struct {
	up    atomic.Bool
	dials atomic.Int64
}

func (f *fakeTunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if !f.up.Load() {
		return nil, tunnel.ErrTunnelDown
	}
	f.dials.Add(1)
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// echoServer accepts connections and echoes bytes back.
func echoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func startServer(t *testing.T, tun *fakeTunnel, g *gate.Gate) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	srv := New(frontend.Base{
		Listen:      addr,
		Dialer:      tun,
		Gate:        g,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialTimeout: 5 * time.Second,
	}, "", "")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()

	// Wait for the listener to be up.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socks5 frontend never came up on %s", addr)
	return ""
}

func TestConnectSucceedsWhileTunnelIsUp(t *testing.T) {
	echo := echoServer(t)
	tun := &fakeTunnel{}
	tun.up.Store(true)
	g := gate.New(context.Background(), true)
	g.Open()

	proxy := startServer(t, tun, g)
	client := &socks.Client{Address: proxy}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := client.DialContext(ctx, "tcp", echo.Addr().String())
	if err != nil {
		t.Fatalf("dial through relay: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo mismatch: %q", buf)
	}
}

func TestClosedGateRefusesAndNeverDials(t *testing.T) {
	echo := echoServer(t)
	tun := &fakeTunnel{}
	tun.up.Store(true) // tunnel is fine; the kill switch is what refuses
	g := gate.New(context.Background(), true)
	// gate deliberately left closed

	proxy := startServer(t, tun, g)
	client := &socks.Client{Address: proxy}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.DialContext(ctx, "tcp", echo.Addr().String())
	if err == nil {
		t.Fatal("relay served a request while the kill switch was closed")
	}
	if tun.dials.Load() != 0 {
		t.Fatalf("kill switch let %d dial(s) reach the egress path", tun.dials.Load())
	}
}

func TestTunnelDownIsRefusedNotBypassed(t *testing.T) {
	echo := echoServer(t)
	tun := &fakeTunnel{} // down
	g := gate.New(context.Background(), true)
	g.Open() // gate open, but the tunnel itself refuses

	proxy := startServer(t, tun, g)
	client := &socks.Client{Address: proxy}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.DialContext(ctx, "tcp", echo.Addr().String()); err == nil {
		t.Fatal("relay fell back to a direct dial when the tunnel was down")
	}
}

func TestStrictKillSwitchTearsDownEstablishedStream(t *testing.T) {
	echo := echoServer(t)
	tun := &fakeTunnel{}
	tun.up.Store(true)
	g := gate.New(context.Background(), true)
	g.Open()

	proxy := startServer(t, tun, g)
	client := &socks.Client{Address: proxy}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := client.DialContext(ctx, "tcp", echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}

	// The tunnel falls over mid-stream.
	tun.up.Store(false)
	g.Close("tunnel down")

	// EOF or a reset both mean the stream was torn down; only a clean read
	// would mean the kill switch failed to bite.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("established stream survived a strict kill switch closing")
	}
}

func TestUDPAssociateIsRefusedRatherThanLeaked(t *testing.T) {
	tun := &fakeTunnel{}
	tun.up.Store(true)
	g := gate.New(context.Background(), true)
	g.Open()

	proxy := startServer(t, tun, g)
	conn, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Write([]byte{socks.Version5, 1, socks.MethodNoAuth}); err != nil {
		t.Fatal(err)
	}
	sel := make([]byte, 2)
	if _, err := io.ReadFull(conn, sel); err != nil {
		t.Fatal(err)
	}

	req := socks.Addr{IP: net.IPv4zero, Port: 0}.Marshal([]byte{socks.Version5, socks.CmdUDPAssociate, 0})
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 3)
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Fatal(err)
	}
	if head[1] != socks.RepCommandNotSupport {
		t.Fatalf("UDP ASSOCIATE answered 0x%02x (%s), want command-not-supported",
			head[1], socks.ReplyText(head[1]))
	}
}

func TestAddrRoundTrip(t *testing.T) {
	for _, in := range []string{"example.com:443", "1.2.3.4:80", "[2001:db8::1]:8080"} {
		a, err := socks.ParseAddr(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		buf := a.Marshal(nil)
		back, err := socks.ReadAddr(bytesReader(buf))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if back.String() != in {
			t.Fatalf("round trip %s -> %s", in, back.String())
		}
	}
}

func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
