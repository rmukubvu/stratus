package backing

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Probe reports nil once the engine at address (host:port) is ready.
//
// A probe must speak enough of the engine's protocol to prove a real server
// answered. Docker's userland proxy accepts connections on a published port
// before the container listens and only then fails the connection, so a bare
// TCP connect says nothing about readiness.
type Probe func(ctx context.Context, address string) error

const probeDialTimeout = time.Second

// TCP is ready as soon as something accepts a connection. It is the fallback
// for engines without a protocol probe; prefer one of the probes below.
func TCP(ctx context.Context, address string) error {
	conn, err := dial(ctx, address)
	if err != nil {
		return err
	}
	return conn.Close()
}

// RedisPing is ready when the engine answers PING with PONG, which rules out
// a server that is still loading.
func RedisPing(ctx context.Context, address string) error {
	conn, err := open(ctx, address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read PING reply: %w", err)
	}
	if strings.TrimSpace(line) != "+PONG" {
		return fmt.Errorf("unexpected PING reply %q", strings.TrimSpace(line))
	}
	return nil
}

// MemcachedVersion is ready when the engine answers the version command.
func MemcachedVersion(ctx context.Context, address string) error {
	conn, err := open(ctx, address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("version\r\n")); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read version reply: %w", err)
	}
	if !strings.HasPrefix(line, "VERSION ") {
		return fmt.Errorf("unexpected version reply %q", strings.TrimSpace(line))
	}
	return nil
}

// PostgresStartup is ready when the server answers a protocol 3.0 startup
// message for user and database with an authentication request. A server
// that is still starting answers with an error instead, and a proxy with
// nothing behind it closes the connection.
func PostgresStartup(user, database string) Probe {
	var params bytes.Buffer
	params.WriteString("user\x00" + user + "\x00database\x00" + database + "\x00\x00")
	startup := make([]byte, 8, 8+params.Len())
	binary.BigEndian.PutUint32(startup[0:4], uint32(8+params.Len()))
	binary.BigEndian.PutUint32(startup[4:8], 196608) // protocol 3.0
	startup = append(startup, params.Bytes()...)

	return func(ctx context.Context, address string) error {
		conn, err := open(ctx, address)
		if err != nil {
			return err
		}
		defer conn.Close()
		if _, err := conn.Write(startup); err != nil {
			return err
		}
		kind := make([]byte, 1)
		if _, err := io.ReadFull(conn, kind); err != nil {
			return fmt.Errorf("read postgres reply: %w", err)
		}
		if kind[0] != 'R' {
			return fmt.Errorf("postgres answered message type %q instead of an authentication request", kind[0])
		}
		return nil
	}
}

// MySQLHandshake is ready when the server sends its initial handshake, which
// MySQL and MariaDB do as soon as a client connects. The packet's fifth byte
// is the protocol version, 10 for every supported server.
func MySQLHandshake(ctx context.Context, address string) error {
	conn, err := open(ctx, address)
	if err != nil {
		return err
	}
	defer conn.Close()
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read mysql handshake: %w", err)
	}
	switch header[4] {
	case 10:
		return nil
	case 0xff:
		return fmt.Errorf("mysql refused the connection: %s", readErrorPacket(conn, header))
	default:
		return fmt.Errorf("unexpected mysql protocol version %d", header[4])
	}
}

// readErrorPacket returns the human-readable part of a MySQL ERR packet whose
// first five bytes have already been read.
func readErrorPacket(conn net.Conn, header []byte) string {
	length := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	rest := make([]byte, length-1)
	n, _ := io.ReadFull(conn, rest)
	if n <= 2 {
		return "unknown error"
	}
	// Skip the two-byte error code and, when present, the '#' and SQL state.
	message := rest[2:n]
	if len(message) > 6 && message[0] == '#' {
		message = message[6:]
	}
	return string(message)
}

// open dials address and bounds the whole exchange by the context deadline,
// or by probeDialTimeout when the context has none.
func open(ctx context.Context, address string) (net.Conn, error) {
	conn, err := dial(ctx, address)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(probeDialTimeout))
	}
	return conn, nil
}

func dial(ctx context.Context, address string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: probeDialTimeout}
	return dialer.DialContext(ctx, "tcp", address)
}
