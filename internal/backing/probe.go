package backing

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// Probe reports nil once the engine at address (host:port) is ready.
type Probe func(ctx context.Context, address string) error

const probeDialTimeout = time.Second

// TCP is ready as soon as the engine accepts a connection. The official
// PostgreSQL, MySQL and MariaDB images only open their TCP port after
// initialisation, so a successful connect means the server is serving.
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
	conn, err := dial(ctx, address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(probeDialTimeout))
	}
	if _, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(line) != "+PONG" {
		return fmt.Errorf("unexpected PING reply %q", strings.TrimSpace(line))
	}
	return nil
}

func dial(ctx context.Context, address string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: probeDialTimeout}
	return dialer.DialContext(ctx, "tcp", address)
}
