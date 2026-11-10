package backing

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
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
	go serveLine(ln, "+PONG\r\n")

	if err := RedisPing(context.Background(), ln.Addr().String()); err != nil {
		t.Fatalf("RedisPing should accept +PONG: %v", err)
	}
}

func TestRedisPingProbeRejectsLoadingServer(t *testing.T) {
	ln := listen(t)
	defer ln.Close()
	go serveLine(ln, "-LOADING Redis is loading the dataset in memory\r\n")

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

func TestMemcachedVersionProbe(t *testing.T) {
	ready := listen(t)
	defer ready.Close()
	go serveLine(ready, "VERSION 1.6.38\r\n")
	if err := MemcachedVersion(context.Background(), ready.Addr().String()); err != nil {
		t.Fatalf("a VERSION reply means ready: %v", err)
	}

	odd := listen(t)
	defer odd.Close()
	go serveLine(odd, "ERROR\r\n")
	if err := MemcachedVersion(context.Background(), odd.Addr().String()); err == nil || !strings.Contains(err.Error(), "ERROR") {
		t.Fatalf("anything but VERSION is not ready: %v", err)
	}
}

func TestPostgresStartupProbe(t *testing.T) {
	probe := PostgresStartup("app", "orders")

	ready := listen(t)
	defer ready.Close()
	go servePostgres(t, ready, 'R')
	if err := probe(context.Background(), ready.Addr().String()); err != nil {
		t.Fatalf("an authentication request means ready: %v", err)
	}

	starting := listen(t)
	defer starting.Close()
	go servePostgres(t, starting, 'E')
	err := probe(context.Background(), starting.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "'E'") {
		t.Fatalf("an error response (database system is starting up) is not ready: %v", err)
	}

	// Docker's userland proxy accepts the connection and then drops it when
	// nothing listens in the container yet. The read error differs by OS
	// (EOF on Linux, a reset on Windows), so only assert where it surfaced.
	proxy := listen(t)
	defer proxy.Close()
	go acceptAndClose(proxy)
	err = probe(context.Background(), proxy.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "read postgres reply") {
		t.Fatalf("a closed connection is not ready: %v", err)
	}
}

func TestMySQLHandshakeProbe(t *testing.T) {
	ready := listen(t)
	defer ready.Close()
	go serveBytes(ready, []byte{0x4a, 0x00, 0x00, 0x00, 10, '8', '.', '4', '.', '0', 0x00})
	if err := MySQLHandshake(context.Background(), ready.Addr().String()); err != nil {
		t.Fatalf("a protocol 10 handshake means ready: %v", err)
	}

	refusing := listen(t)
	defer refusing.Close()
	errPacket := append([]byte{0, 0, 0, 0x00, 0xff, 0x69, 0x04, '#', 'H', 'Y', '0', '0', '0'}, []byte("Host is blocked")...)
	errPacket[0] = byte(len(errPacket) - 4)
	go serveBytes(refusing, errPacket)
	err := MySQLHandshake(context.Background(), refusing.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "Host is blocked") {
		t.Fatalf("an ERR packet is not ready and carries the server's message: %v", err)
	}

	proxy := listen(t)
	defer proxy.Close()
	go acceptAndClose(proxy)
	if err := MySQLHandshake(context.Background(), proxy.Addr().String()); err == nil {
		t.Fatal("a connection closed before the handshake is not ready")
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

// serveLine answers the first line of every connection with reply.
func serveLine(ln net.Listener, reply string) {
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

// serveBytes writes payload to every connection as soon as it opens, the
// way MySQL sends its handshake.
func serveBytes(ln net.Listener, payload []byte) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_, _ = conn.Write(payload)
			_, _ = io.Copy(io.Discard, conn)
		}()
	}
}

// servePostgres reads a startup message and answers with one message of
// the given type.
func servePostgres(t *testing.T, ln net.Listener, kind byte) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			header := make([]byte, 8)
			if _, err := io.ReadFull(conn, header); err != nil {
				return
			}
			if header[4] != 0 || header[5] != 3 || header[6] != 0 || header[7] != 0 {
				t.Errorf("probe should send a protocol 3.0 startup message, got %v", header[4:])
				return
			}
			body := make([]byte, int(binary.BigEndian.Uint32(header[0:4]))-8)
			if _, err := io.ReadFull(conn, body); err != nil {
				return
			}
			if !strings.Contains(string(body), "user\x00app\x00database\x00orders\x00") {
				t.Errorf("startup message should carry the user and database: %q", body)
			}
			_, _ = conn.Write([]byte{kind, 0, 0, 0, 8, 0, 0, 0, 0})
		}()
	}
}

// acceptAndClose behaves like Docker's userland proxy with no server behind
// it: the connection opens and is closed at once.
func acceptAndClose(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Close()
	}
}

func TestRedisSaveAcceptsOK(t *testing.T) {
	ln := listen(t)
	defer ln.Close()
	go serveLine(ln, "+OK\r\n")

	if err := RedisSave(context.Background(), ln.Addr().String()); err != nil {
		t.Fatalf("RedisSave should accept +OK: %v", err)
	}
}

func TestRedisSaveReportsEngineError(t *testing.T) {
	ln := listen(t)
	defer ln.Close()
	go serveLine(ln, "-ERR Background save already in progress\r\n")

	err := RedisSave(context.Background(), ln.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "Background save") {
		t.Fatalf("RedisSave should surface the engine's error, got %v", err)
	}
}

// serveKafka reads one length-prefixed request and answers with a Metadata
// v1 reply built by reply from the request's correlation id.
func serveKafka(ln net.Listener, reply func(correlation uint32) []byte) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	size := make([]byte, 4)
	if _, err := io.ReadFull(conn, size); err != nil {
		return
	}
	req := make([]byte, binary.BigEndian.Uint32(size))
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	resp := reply(binary.BigEndian.Uint32(req[4:8]))
	_, _ = conn.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(resp))), resp...))
}

// metadataReply encodes a Metadata v1 response with one broker per entry of
// brokers and the given controller.
func metadataReply(correlation uint32, brokers int, controller int32) []byte {
	out := binary.BigEndian.AppendUint32(nil, correlation)
	out = binary.BigEndian.AppendUint32(out, uint32(brokers))
	for i := 0; i < brokers; i++ {
		out = binary.BigEndian.AppendUint32(out, uint32(i))
		out = binary.BigEndian.AppendUint16(out, 9)
		out = append(out, "127.0.0.1"...)
		out = binary.BigEndian.AppendUint32(out, 9092)
		out = binary.BigEndian.AppendUint16(out, 0xFFFF) // null rack
	}
	out = binary.BigEndian.AppendUint32(out, uint32(controller))
	return binary.BigEndian.AppendUint32(out, 0) // no topics
}

func TestKafkaControllerAcceptsBrokerWithController(t *testing.T) {
	ln := listen(t)
	defer ln.Close()
	go serveKafka(ln, func(c uint32) []byte { return metadataReply(c, 1, 0) })

	if err := KafkaController(context.Background(), ln.Addr().String()); err != nil {
		t.Fatalf("a broker with a controller is ready: %v", err)
	}
}

func TestKafkaControllerRejectsUnreadyOrForeignReplies(t *testing.T) {
	cases := map[string]struct {
		reply func(uint32) []byte
		want  string
	}{
		"no controller":    {func(c uint32) []byte { return metadataReply(c, 1, -1) }, "controller"},
		"no brokers":       {func(c uint32) []byte { return metadataReply(c, 0, 0) }, "no brokers"},
		"wrong request id": {func(uint32) []byte { return metadataReply(99, 1, 0) }, "correlation"},
		"truncated":        {func(c uint32) []byte { return metadataReply(c, 1, 0)[:10] }, "decode"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ln := listen(t)
			defer ln.Close()
			go serveKafka(ln, tc.reply)

			err := KafkaController(context.Background(), ln.Addr().String())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestFreeLoopbackPortCanBeBound(t *testing.T) {
	port, err := FreeLoopbackPort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d should be free: %v", port, err)
	}
	ln.Close()
}
