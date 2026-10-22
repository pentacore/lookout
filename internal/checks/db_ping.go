package checks

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pentacore/lookout/internal/wire"
)

const (
	dbPingDefaultTimeout = 5 * time.Second
	dbPingMinTimeout     = time.Second
	// dbPingMaxTimeout keeps the check inside the runner's 10 s budget.
	dbPingMaxTimeout = 9 * time.Second

	// mysqlMaxPacket bounds the server greeting we are willing to buffer.
	mysqlMaxPacket = 64 << 10
	// dbPingMaxServerText bounds server-supplied text echoed into a message.
	dbPingMaxServerText = 200
)

var errOversizedPacket = errors.New("oversized packet")

// postgresSSLRequest is the length-8 startup message with code 80877103.
var postgresSSLRequest = []byte{0, 0, 0, 8, 0x04, 0xD2, 0x16, 0x2F}

// mysqlQuit is COM_QUIT as a complete packet (length 1, sequence 0).
var mysqlQuit = []byte{1, 0, 0, 0, 1}

type dbPingParams struct {
	Engine         string  `json:"engine"`
	Host           string  `json:"host"`
	Port           int     `json:"port"`
	TimeoutSeconds float64 `json:"timeout_seconds"`
}

// effectiveTimeout turns the configured timeout into the deadline for dial
// plus exchange: 5 s by default, clamped to [1 s, 9 s].
func effectiveTimeout(seconds float64) time.Duration {
	if seconds <= 0 || math.IsNaN(seconds) {
		return dbPingDefaultTimeout
	}

	timeout := time.Duration(seconds * float64(time.Second))

	return min(max(timeout, dbPingMinTimeout), dbPingMaxTimeout)
}

// dbPing checks that a database server answers at the protocol level without
// credentials. Observed is never nil.
func dbPing(ctx context.Context, env Env, raw json.RawMessage) Outcome {
	params, err := decodeParams[dbPingParams](raw)
	if err != nil {
		return withObserved(invalidParams(err))
	}

	switch {
	case params.Engine == "":
		return withObserved(missingParam("engine"))
	case params.Host == "":
		return withObserved(missingParam("host"))
	case params.Port == 0:
		return withObserved(missingParam("port"))
	}

	var exchange func(ctx context.Context, conn net.Conn) (wire.CheckStatus, string)

	switch params.Engine {
	case "postgres":
		exchange = postgresExchange
	case "mysql":
		exchange = mysqlExchange
	default:
		return withObserved(Outcome{
			Status:  wire.StatusError,
			Message: fmt.Sprintf("unsupported engine %q", params.Engine),
		})
	}

	ctx, cancel := context.WithTimeout(ctx, effectiveTimeout(params.TimeoutSeconds))
	defer cancel()

	addr := net.JoinHostPort(params.Host, strconv.Itoa(params.Port))
	start := time.Now()

	conn, err := env.Dial(ctx, "tcp", addr)
	if err != nil {
		return pingOutcome(start, wire.StatusCrit, fmt.Sprintf("cannot connect to %s: %v", addr, err))
	}

	defer func() { _ = conn.Close() }()

	// The deadline covers dial plus exchange; closing on cancellation also
	// aborts reads when the caller gives up before the deadline.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	status, message := exchange(ctx, conn)

	return pingOutcome(start, status, message)
}

func withObserved(outcome Outcome) Outcome {
	outcome.Observed = map[string]any{}

	return outcome
}

// pingOutcome stamps the time since start, rounded to 0.1 ms, onto an outcome.
func pingOutcome(start time.Time, status wire.CheckStatus, message string) Outcome {
	latency := math.Round(float64(time.Since(start).Microseconds())/100) / 10

	return Outcome{
		Status:   status,
		Message:  message,
		Observed: map[string]any{"latency_ms": latency},
	}
}

// postgresExchange sends an SSLRequest and reads the one-byte answer.
func postgresExchange(ctx context.Context, conn net.Conn) (wire.CheckStatus, string) {
	if _, err := conn.Write(postgresSSLRequest); err != nil {
		return wire.StatusCrit, fmt.Sprintf("cannot send SSLRequest: %v", err)
	}

	answer := make([]byte, 1)
	if _, err := io.ReadFull(conn, answer); err != nil {
		return wire.StatusCrit, fmt.Sprintf("no answer to SSLRequest: %v", err)
	}

	switch answer[0] {
	case 'N':
		return wire.StatusOK, "postgres answered: SSL not offered"
	case 'S':
		finishPostgresTLS(ctx, conn)

		return wire.StatusOK, "postgres answered: SSL offered"
	default:
		return wire.StatusCrit, fmt.Sprintf("unexpected response 0x%02x to SSLRequest", answer[0])
	}
}

// finishPostgresTLS runs a best-effort TLS handshake so a TLS-enabled server
// sees a clean connection instead of logging "could not accept SSL
// connection" on every check. Certificate verification is skipped on purpose:
// no data is ever sent over this connection, so there is nothing for a forged
// certificate to intercept. Handshake failures are ignored.
func finishPostgresTLS(ctx context.Context, conn net.Conn) {
	client := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})

	_ = client.HandshakeContext(ctx)
}

// mysqlExchange reads the server greeting, then logs in as a fixed
// non-existent user and quits so the server does not count an aborted
// connect against max_connect_errors.
func mysqlExchange(_ context.Context, conn net.Conn) (wire.CheckStatus, string) {
	seq, payload, err := readMySQLPacket(conn)
	if errors.Is(err, errOversizedPacket) {
		return wire.StatusCrit, errOversizedPacket.Error()
	}

	if err != nil {
		return wire.StatusCrit, fmt.Sprintf("no mysql greeting: %v", err)
	}

	if len(payload) == 0 {
		return wire.StatusCrit, "empty mysql greeting"
	}

	switch payload[0] {
	case 0x0a:
		version, _, _ := strings.Cut(string(payload[1:]), "\x00")
		message := "mysql protocol 10, server " + printableServerText(version)

		mysqlLoginAndQuit(conn, seq)

		return wire.StatusOK, message
	case 0xff:
		return wire.StatusOK, mysqlErrorMessage(payload)
	default:
		return wire.StatusCrit, fmt.Sprintf("unexpected mysql greeting 0x%02x", payload[0])
	}
}

// mysqlErrorMessage renders an ERR packet: 0xff, little-endian code, text.
func mysqlErrorMessage(payload []byte) string {
	if len(payload) < 3 {
		return "mysql answered with a truncated error"
	}

	code := binary.LittleEndian.Uint16(payload[1:3])

	return fmt.Sprintf("mysql answered with error %d: %s", code, printableServerText(string(payload[3:])))
}

// printableServerText bounds and sanitises text the server sent.
func printableServerText(text string) string {
	if len(text) > dbPingMaxServerText {
		text = text[:dbPingMaxServerText]
	}

	return strings.ToValidUTF8(text, "?")
}

// mysqlLoginAndQuit sends a HandshakeResponse41 for the user "lookout" with
// no password, answers an auth switch with an empty packet, and sends
// COM_QUIT. The status is already decided, so every error is ignored.
func mysqlLoginAndQuit(conn net.Conn, greetingSeq byte) {
	if _, err := conn.Write(mysqlPacket(greetingSeq+1, mysqlLoginPayload())); err != nil {
		return
	}

	seq, reply, err := readMySQLPacket(conn)
	if err != nil {
		return
	}

	if len(reply) > 0 && reply[0] == 0xfe {
		if _, err := conn.Write(mysqlPacket(seq+1, nil)); err != nil {
			return
		}

		if _, _, err := readMySQLPacket(conn); err != nil {
			return
		}
	}

	_, _ = conn.Write(mysqlQuit)
}

// mysqlLoginPayload builds the HandshakeResponse41 body: capabilities
// LONG_PASSWORD|PROTOCOL_41|SECURE_CONNECTION|PLUGIN_AUTH, max packet size
// 16 MiB, utf8 collation, the user, an empty auth response and the plugin.
func mysqlLoginPayload() []byte {
	var payload bytes.Buffer

	payload.Write(binary.LittleEndian.AppendUint32(nil, 0x00088201))
	payload.Write(binary.LittleEndian.AppendUint32(nil, 16777216))
	payload.WriteByte(0x21)
	payload.Write(make([]byte, 23))
	payload.WriteString("lookout\x00")
	payload.WriteByte(0)
	payload.WriteString("mysql_native_password\x00")

	return payload.Bytes()
}

// mysqlPacket frames a payload: 3-byte little-endian length, sequence id.
func mysqlPacket(seq byte, payload []byte) []byte {
	length := len(payload)

	packet := make([]byte, 0, 4+length)
	packet = append(packet, byte(length), byte(length>>8), byte(length>>16), seq)

	return append(packet, payload...)
}

// readMySQLPacket reads one packet and returns its sequence id and payload.
func readMySQLPacket(r io.Reader) (byte, []byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}

	length := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	if length > mysqlMaxPacket {
		return 0, nil, errOversizedPacket
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}

	return header[3], payload, nil
}
