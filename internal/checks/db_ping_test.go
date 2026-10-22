package checks

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/pentacore/lookout/internal/wire"
)

// fakeServer listens on a loopback port and runs handler for each accepted
// connection. Handler failures are reported with t.Errorf.
type fakeServer struct {
	addr *net.TCPAddr
	done chan struct{}
}

func startFakeServer(t *testing.T, handler func(conn net.Conn)) *fakeServer {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	server := &fakeServer{
		addr: listener.Addr().(*net.TCPAddr),
		done: make(chan struct{}),
	}

	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		defer close(server.done)

		conn, err := listener.Accept()
		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		handler(conn)
	}()

	return server
}

// wait blocks until the handler has returned.
func (s *fakeServer) wait(t *testing.T) {
	t.Helper()

	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Error("fake server handler did not finish")
	}
}

func (s *fakeServer) params(engine string) string {
	return fmt.Sprintf(`{"engine":%q,"host":"127.0.0.1","port":%d}`, engine, s.addr.Port)
}

func dialEnv() Env {
	dialer := &net.Dialer{}

	return Env{Dial: dialer.DialContext}
}

func assertLatency(t *testing.T, got Outcome) {
	t.Helper()

	latency, ok := got.Observed["latency_ms"].(float64)
	if !ok {
		t.Fatalf("observed = %#v, want a float64 latency_ms", got.Observed)
	}

	if latency < 0 {
		t.Errorf("latency_ms = %v, want >= 0", latency)
	}

	if tenths := latency * 10; tenths != float64(int64(tenths+0.5)) {
		t.Errorf("latency_ms = %v, want rounding to 0.1", latency)
	}
}

func TestPostgresPingN(t *testing.T) {
	server := startFakeServer(t, func(conn net.Conn) {
		got := make([]byte, 8)
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Errorf("read SSLRequest: %v", err)

			return
		}

		if want := []byte{0, 0, 0, 8, 0x04, 0xD2, 0x16, 0x2F}; !bytes.Equal(got, want) {
			t.Errorf("SSLRequest = % x, want % x", got, want)
		}

		_, _ = conn.Write([]byte("N"))
	})

	got := runCheck(dbPing, dialEnv(), server.params("postgres"))
	server.wait(t)

	if got.Status != wire.StatusOK {
		t.Fatalf("status = %q (%s), want ok", got.Status, got.Message)
	}

	assertLatency(t, got)
}

func TestPostgresPingSThenClose(t *testing.T) {
	server := startFakeServer(t, func(conn net.Conn) {
		if _, err := io.ReadFull(conn, make([]byte, 8)); err != nil {
			t.Errorf("read SSLRequest: %v", err)

			return
		}

		_, _ = conn.Write([]byte("S"))
	})

	got := runCheck(dbPing, dialEnv(), server.params("postgres"))
	server.wait(t)

	if got.Status != wire.StatusOK {
		t.Fatalf("status = %q (%s), want ok", got.Status, got.Message)
	}

	assertLatency(t, got)
}

func TestPostgresPingSAttemptsTLSHandshake(t *testing.T) {
	hello := make(chan byte, 1)

	server := startFakeServer(t, func(conn net.Conn) {
		if _, err := io.ReadFull(conn, make([]byte, 8)); err != nil {
			t.Errorf("read SSLRequest: %v", err)

			return
		}

		_, _ = conn.Write([]byte("S"))

		first := make([]byte, 1)
		if _, err := io.ReadFull(conn, first); err == nil {
			hello <- first[0]
		}
	})

	got := runCheck(dbPing, dialEnv(), server.params("postgres"))
	server.wait(t)

	if got.Status != wire.StatusOK {
		t.Fatalf("status = %q (%s), want ok", got.Status, got.Message)
	}

	select {
	case first := <-hello:
		if first != 0x16 {
			t.Errorf("first byte after 'S' = %#x, want TLS handshake record 0x16", first)
		}
	default:
		t.Error("client never started a TLS handshake after 'S'")
	}
}

func TestPostgresGarbage(t *testing.T) {
	server := startFakeServer(t, func(conn net.Conn) {
		_, _ = io.ReadFull(conn, make([]byte, 8))
		_, _ = conn.Write([]byte("X"))
	})

	got := runCheck(dbPing, dialEnv(), server.params("postgres"))
	server.wait(t)

	checkCase{
		name:        "garbage",
		wantStatus:  wire.StatusCrit,
		wantMessage: "unexpected response 0x58 to SSLRequest",
	}.assert(t, got)
	assertLatency(t, got)
}

func mysqlGreeting(version string) []byte {
	payload := append([]byte{0x0a}, version...)
	payload = append(payload, 0)
	// Thread id, scrambles and capability flags are irrelevant to the check.
	payload = append(payload, make([]byte, 20)...)

	return mysqlPacket(0, payload)
}

func mysqlERR(seq byte, code uint16, text string) []byte {
	payload := binary.LittleEndian.AppendUint16([]byte{0xff}, code)
	payload = append(payload, text...)

	return mysqlPacket(seq, payload)
}

// serverReadPacket reads one client packet and returns its sequence id and payload.
func serverReadPacket(t *testing.T, conn net.Conn) (byte, []byte, bool) {
	t.Helper()

	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Errorf("read packet header: %v", err)

		return 0, nil, false
	}

	payload := make([]byte, int(header[0])|int(header[1])<<8|int(header[2])<<16)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Errorf("read packet payload: %v", err)

		return 0, nil, false
	}

	return header[3], payload, true
}

// expectQuit asserts that the next bytes on conn are exactly COM_QUIT.
func expectQuit(t *testing.T, conn net.Conn) {
	t.Helper()

	got := make([]byte, 5)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Errorf("read COM_QUIT: %v", err)

		return
	}

	if want := []byte{1, 0, 0, 0, 1}; !bytes.Equal(got, want) {
		t.Errorf("after login got % x, want COM_QUIT % x", got, want)
	}
}

func TestMySQLHandshakeV10SendsLoginAndQuit(t *testing.T) {
	server := startFakeServer(t, func(conn net.Conn) {
		_, _ = conn.Write(mysqlGreeting("8.0.36"))

		seq, payload, ok := serverReadPacket(t, conn)
		if !ok {
			return
		}

		if seq != 1 {
			t.Errorf("login seq = %d, want 1", seq)
		}

		if len(payload) < 4 || !bytes.Equal(payload[:4], []byte{0x01, 0x82, 0x08, 0x00}) {
			t.Errorf("login capability flags = % x, want 01 82 08 00", payload)
		}

		if !bytes.Contains(payload, []byte("lookout\x00")) {
			t.Errorf("login payload %q lacks the fixed user", payload)
		}

		if !bytes.Contains(payload, []byte("mysql_native_password\x00")) {
			t.Errorf("login payload %q lacks the auth plugin", payload)
		}

		_, _ = conn.Write(mysqlERR(2, 1045, "#28000Access denied for user 'lookout'"))

		expectQuit(t, conn)
	})

	got := runCheck(dbPing, dialEnv(), server.params("mysql"))
	server.wait(t)

	checkCase{
		name:         "v10",
		wantStatus:   wire.StatusOK,
		wantContains: "8.0.36",
	}.assert(t, got)
	assertLatency(t, got)
}

func TestMySQLAuthSwitch(t *testing.T) {
	server := startFakeServer(t, func(conn net.Conn) {
		_, _ = conn.Write(mysqlGreeting("10.11.6-MariaDB"))

		if _, _, ok := serverReadPacket(t, conn); !ok {
			return
		}

		_, _ = conn.Write(mysqlPacket(2, append([]byte{0xfe}, "mysql_clear_password\x00"...)))

		seq, payload, ok := serverReadPacket(t, conn)
		if !ok {
			return
		}

		if seq != 3 || len(payload) != 0 {
			t.Errorf("auth switch answer = seq %d payload %q, want seq 3 and an empty payload", seq, payload)
		}

		_, _ = conn.Write(mysqlERR(4, 1045, "denied"))

		expectQuit(t, conn)
	})

	got := runCheck(dbPing, dialEnv(), server.params("mysql"))
	server.wait(t)

	checkCase{
		name:         "auth switch",
		wantStatus:   wire.StatusOK,
		wantContains: "10.11.6-MariaDB",
	}.assert(t, got)
}

func TestMySQLLoginIgnoresClosedConnection(t *testing.T) {
	server := startFakeServer(t, func(conn net.Conn) {
		_, _ = conn.Write(mysqlGreeting("8.0.36"))
	})

	got := runCheck(dbPing, dialEnv(), server.params("mysql"))
	server.wait(t)

	checkCase{name: "closed after greeting", wantStatus: wire.StatusOK, wantContains: "8.0.36"}.assert(t, got)
}

func TestMySQLErrPacket(t *testing.T) {
	server := startFakeServer(t, func(conn net.Conn) {
		_, _ = conn.Write(mysqlERR(0, 1129, "Host 'x' is blocked because of many connection errors"))

		// An ERR greeting ends the exchange; the client must not send a login.
		extra, _ := io.ReadAll(conn)
		if len(extra) != 0 {
			t.Errorf("client sent % x after an ERR greeting, want nothing", extra)
		}
	})

	got := runCheck(dbPing, dialEnv(), server.params("mysql"))
	server.wait(t)

	checkCase{name: "err greeting", wantStatus: wire.StatusOK, wantContains: "1129"}.assert(t, got)

	if want := "is blocked"; !bytes.Contains([]byte(got.Message), []byte(want)) {
		t.Errorf("message = %q, want it to contain %q", got.Message, want)
	}

	assertLatency(t, got)
}

func TestMySQLGarbage(t *testing.T) {
	server := startFakeServer(t, func(conn net.Conn) {
		_, _ = conn.Write(mysqlPacket(0, []byte{0x09, 1, 2, 3}))
		_, _ = io.ReadAll(conn)
	})

	got := runCheck(dbPing, dialEnv(), server.params("mysql"))
	server.wait(t)

	checkCase{
		name:        "garbage",
		wantStatus:  wire.StatusCrit,
		wantMessage: "unexpected mysql greeting 0x09",
	}.assert(t, got)
	assertLatency(t, got)
}

func TestMySQLEmptyAndOversizedGreeting(t *testing.T) {
	tests := []struct {
		name        string
		packet      []byte
		wantMessage string
	}{
		{name: "empty", packet: mysqlPacket(0, nil), wantMessage: "empty mysql greeting"},
		{name: "oversized", packet: []byte{0x01, 0x00, 0x01, 0x00}, wantMessage: "oversized packet"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := startFakeServer(t, func(conn net.Conn) {
				_, _ = conn.Write(tt.packet)
				_, _ = io.ReadAll(conn)
			})

			got := runCheck(dbPing, dialEnv(), server.params("mysql"))
			server.wait(t)

			checkCase{name: tt.name, wantStatus: wire.StatusCrit, wantMessage: tt.wantMessage}.assert(t, got)
		})
	}
}

func TestDBPingRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	for _, engine := range []string{"postgres", "mysql"} {
		params := fmt.Sprintf(`{"engine":%q,"host":"127.0.0.1","port":%d}`, engine, port)

		got := runCheck(dbPing, dialEnv(), params)

		checkCase{name: engine, wantStatus: wire.StatusCrit, wantContains: "127.0.0.1:" + strconv.Itoa(port)}.assert(t, got)

		if got.Observed == nil {
			t.Errorf("%s: observed is nil, want a non-nil map", engine)
		}
	}
}

func TestDBPingTimeout(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			server := startFakeServer(t, func(conn net.Conn) {
				_, _ = io.Copy(io.Discard, conn)
			})

			params := fmt.Sprintf(`{"engine":%q,"host":"127.0.0.1","port":%d,"timeout_seconds":1}`, engine, server.addr.Port)

			start := time.Now()
			got := runCheck(dbPing, dialEnv(), params)

			if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
				t.Errorf("took %v, want under 1.5s", elapsed)
			}

			if got.Status != wire.StatusCrit {
				t.Errorf("status = %q (%s), want crit", got.Status, got.Message)
			}

			if got.Observed == nil {
				t.Error("observed is nil, want a non-nil map")
			}
		})
	}
}

func TestDBPingCancelledContext(t *testing.T) {
	server := startFakeServer(t, func(conn net.Conn) {
		_, _ = io.Copy(io.Discard, conn)
	})

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	got := dbPing(ctx, dialEnv(), []byte(server.params("mysql")))

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v after cancel, want a prompt return", elapsed)
	}

	if got.Status != wire.StatusCrit {
		t.Errorf("status = %q (%s), want crit", got.Status, got.Message)
	}
}

func TestDBPingParams(t *testing.T) {
	tests := []checkCase{
		{
			name:        "unknown engine",
			params:      `{"engine":"redis","host":"127.0.0.1","port":6379}`,
			wantStatus:  wire.StatusError,
			wantMessage: `unsupported engine "redis"`,
		},
		{
			name:        "missing engine",
			params:      `{"host":"127.0.0.1","port":5432}`,
			wantStatus:  wire.StatusError,
			wantMessage: "missing required param: engine",
		},
		{
			name:        "missing host",
			params:      `{"engine":"postgres","port":5432}`,
			wantStatus:  wire.StatusError,
			wantMessage: "missing required param: host",
		},
		{
			name:        "missing port",
			params:      `{"engine":"postgres","host":"127.0.0.1"}`,
			wantStatus:  wire.StatusError,
			wantMessage: "missing required param: port",
		},
		{
			name:         "invalid params",
			params:       `{"engine":"postgres","host":"127.0.0.1","port":"abc"}`,
			wantStatus:   wire.StatusError,
			wantContains: "invalid params",
		},
		{
			name:        "null params",
			params:      `null`,
			wantStatus:  wire.StatusError,
			wantMessage: "missing required param: engine",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runCheck(dbPing, dialEnv(), tt.params)

			tt.assert(t, got)

			if got.Observed == nil {
				t.Error("observed is nil, want a non-nil map")
			}
		})
	}
}

func TestEffectiveTimeout(t *testing.T) {
	tests := []struct {
		seconds float64
		want    time.Duration
	}{
		{0, 5 * time.Second},
		{-3, 5 * time.Second},
		{0.2, time.Second},
		{1, time.Second},
		{3, 3 * time.Second},
		{2.5, 2500 * time.Millisecond},
		{9, 9 * time.Second},
		{60, 9 * time.Second},
	}

	for _, tt := range tests {
		if got := effectiveTimeout(tt.seconds); got != tt.want {
			t.Errorf("effectiveTimeout(%v) = %v, want %v", tt.seconds, got, tt.want)
		}
	}
}

func TestDefaultRegistryHasDBPing(t *testing.T) {
	if DefaultRegistry()["db_ping"] == nil {
		t.Error(`DefaultRegistry has no "db_ping"`)
	}
}
