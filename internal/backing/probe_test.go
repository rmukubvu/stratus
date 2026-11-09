package backing

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTCPProbeSucceedsAgainstListener(t *testing.T) {
	ln := listen(t)
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
	}()

	if err := TCP(context.Background(), ln.Addr().String()); err != nil {
		t.Fatalf("TCP probe failed against live listener: %v", err)
	}
}

func TestTCPProbeFailsWhenNothingListens(t *testing.T) {
	ln := listen(t)
	addr := ln.Addr().String()
	ln.Close()

	if err := TCP(context.Background(), addr); err == nil {
		t.Fatal("TCP probe should fail when nothing listens")
	}
}

func TestRedisPingProbeAcceptsPong(t *testing.T) {
	ln := listen(t)
	defer ln.Close()
	go serveRedis(ln, "+PONG\r\n")

	if err := RedisPing(context.Background(), ln.Addr().String()); err != nil {
		t.Fatalf("RedisPing should accept +PONG: %v", err)
	}
}

func TestRedisPingProbeRejectsLoadingServer(t *testing.T) {
	ln := listen(t)
	defer ln.Close()
	go serveRedis(ln, "-LOADING Redis is loading the dataset in memory\r\n")

	err := RedisPing(context.Background(), ln.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "LOADING") {
		t.Fatalf("RedisPing should reject a loading server, got %v", err)
	}
}

func TestRedisPingProbeHonoursContextDeadline(t *testing.T) {
	ln := listen(t)
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			// Accept and never answer.
			time.Sleep(2 * time.Second)
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := RedisPing(ctx, ln.Addr().String()); err == nil {
		t.Fatal("RedisPing should fail when the server never answers")
	}
	if time.Since(start) > time.Second {
		t.Fatal("RedisPing ignored the context deadline")
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// serveRedis answers the first line of every connection with reply.
func serveRedis(ln net.Listener, reply string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
				return
			}
			_, _ = conn.Write([]byte(reply))
		}()
	}
}
