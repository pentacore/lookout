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
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

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

	var exchange func(session dbSession) pingResult

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

	timeout := effectiveTimeout(params.TimeoutSeconds)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	addr := net.JoinHostPort(params.Host, strconv.Itoa(params.Port))
	start := time.Now()

	session := dbSession{ctx: ctx, timeout: timeout}

	conn, err := env.Dial(ctx, "tcp", addr)
	if err != nil {
		return pingOutcome(start, session.failure("cannot connect to "+addr, err))
	}

	session.conn = conn

	defer func() { _ = conn.Close() }()

	// The deadline covers dial plus exchange; closing on cancellation also
	// aborts reads when the caller gives up before the deadline.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	return pingOutcome(start, exchange(session))
}

// dbSession is one open connection plus the deadline it runs under.
type dbSession struct {
	ctx     context.Context
	conn    net.Conn
	timeout time.Duration
}

// pingResult is a decided status. decidedAt is when the deciding response
// arrived, so work done afterwards (logins, TLS, quit) does not count as
// latency.
type pingResult struct {
	status    wire.CheckStatus
	message   string
	decidedAt time.Time
}

func decided(status wire.CheckStatus, message string) pingResult {
	return pingResult{status: status, message: message, decidedAt: time.Now()}
}

// failure is a crit result for an I/O error. Deadline expiry and caller
// cancellation get one stable message instead of a platform-specific i/o text.
func (s dbSession) failure(what string, err error) pingResult {
	switch {
	case errors.Is(s.ctx.Err(), context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return decided(wire.StatusCrit, fmt.Sprintf("%s: timed out after %gs", what, s.timeout.Seconds()))
	case errors.Is(s.ctx.Err(), context.Canceled):
		return decided(wire.StatusCrit, what+": cancelled")
	default:
		return decided(wire.StatusCrit, fmt.Sprintf("%s: %v", what, err))
	}
}

func withObserved(outcome Outcome) Outcome {
	outcome.Observed = map[string]any{}

	return outcome
}

// pingOutcome turns a result into an Outcome whose latency_ms runs from start
// to the deciding response, rounded to 0.1 ms.
func pingOutcome(start time.Time, result pingResult) Outcome {
	latency := math.Round(float64(result.decidedAt.Sub(start).Microseconds())/100) / 10

	return Outcome{
		Status:   result.status,
		Message:  result.message,
		Observed: map[string]any{"latency_ms": latency},
	}
}

// postgresExchange sends an SSLRequest and reads the one-byte answer.
func postgresExchange(session dbSession) pingResult {
	if _, err := session.conn.Write(postgresSSLRequest); err != nil {
		return session.failure("cannot send SSLRequest", err)
	}

	answer := make([]byte, 1)
	if _, err := io.ReadFull(session.conn, answer); err != nil {
		return session.failure("no answer to SSLRequest", err)
	}

	switch answer[0] {
	case 'N':
		return decided(wire.StatusOK, "postgres answered: SSL not offered")
	case 'S':
		result := decided(wire.StatusOK, "postgres answered: SSL offered")

		finishPostgresTLS(session)

		return result
	default:
		return decided(wire.StatusCrit, fmt.Sprintf("unexpected response 0x%02x to SSLRequest", answer[0]))
	}
}

// finishPostgresTLS runs a best-effort TLS handshake, then closes the TLS
// session cleanly, so a TLS-enabled server sees an orderly shutdown instead of
// logging "could not accept SSL connection" or "Connection reset by peer" on
// every check. Certificate verification is skipped on purpose: no data is ever
// sent over this connection, so there is nothing for a forged certificate to
// intercept. Failures are ignored.
func finishPostgresTLS(session dbSession) {
	client := tls.Client(session.conn, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})

	if client.HandshakeContext(session.ctx) == nil {
		_ = client.Close()
	}
}

// mysqlExchange reads the server greeting, then logs in as a fixed
// non-existent user and quits so the server does not count an aborted
// connect against max_connect_errors.
func mysqlExchange(session dbSession) pingResult {
	seq, payload, err := readMySQLPacket(session.conn)
	if errors.Is(err, errOversizedPacket) {
		return decided(wire.StatusCrit, errOversizedPacket.Error())
	}

	if err != nil {
		return session.failure("no mysql greeting", err)
	}

	if len(payload) == 0 {
		return decided(wire.StatusCrit, "empty mysql greeting")
	}

	switch payload[0] {
	case 0x0a:
		version, _, _ := strings.Cut(string(payload[1:]), "\x00")
		result := decided(wire.StatusOK, "mysql protocol 10, server "+printableServerText(version))

		mysqlLoginAndQuit(session.conn, seq)

		return result
	case 0xff:
		return decided(wire.StatusOK, mysqlErrorMessage(payload))
	default:
		return decided(wire.StatusCrit, fmt.Sprintf("unexpected mysql greeting 0x%02x", payload[0]))
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

// printableServerText bounds and sanitises text the server sent: valid UTF-8
// only, control characters replaced by spaces.
func printableServerText(text string) string {
	if len(text) > dbPingMaxServerText {
		text = text[:dbPingMaxServerText]
	}

	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}

		return r
	}, strings.ToValidUTF8(text, "?"))
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
