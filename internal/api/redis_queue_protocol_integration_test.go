package api

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
)

func startRedisMuxListener(t *testing.T, server *Server) (addr string, stop func()) {
	t.Helper()

	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("failed to listen: %v", errListen)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.acceptMuxConnections(listener, nil)
	}()

	stop = func() {
		_ = listener.Close()
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("accept loop returned unexpected error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("timeout waiting for accept loop to exit")
		}
	}

	return listener.Addr().String(), stop
}

func writeTestRESPCommand(conn net.Conn, args ...string) error {
	if conn == nil {
		return net.ErrClosed
	}
	if len(args) == 0 {
		return nil
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&buf, "$%d\r\n%s\r\n", len(arg), arg)
	}
	_, err := conn.Write(buf.Bytes())
	return err
}

func readTestRESPLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(line, "\r\n") {
		return "", fmt.Errorf("invalid RESP line terminator: %q", line)
	}
	return strings.TrimSuffix(line, "\r\n"), nil
}

func readTestRESPError(r *bufio.Reader) (string, error) {
	prefix, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	if prefix != '-' {
		return "", fmt.Errorf("expected error prefix '-', got %q", prefix)
	}
	return readTestRESPLine(r)
}

func readTestRESPSimpleString(r *bufio.Reader) (string, error) {
	prefix, errRead := r.ReadByte()
	if errRead != nil {
		return "", errRead
	}
	if prefix != '+' {
		return "", fmt.Errorf("expected simple string prefix '+', got %q", prefix)
	}
	return readTestRESPLine(r)
}

func readTestRESPBulkString(r *bufio.Reader) ([]byte, error) {
	prefix, errRead := r.ReadByte()
	if errRead != nil {
		return nil, errRead
	}
	if prefix != '$' {
		return nil, fmt.Errorf("expected bulk string prefix '$', got %q", prefix)
	}

	line, errLine := readTestRESPLine(r)
	if errLine != nil {
		return nil, errLine
	}
	length, errParse := strconv.Atoi(line)
	if errParse != nil {
		return nil, fmt.Errorf("invalid bulk string length %q: %v", line, errParse)
	}
	if length == -1 {
		return nil, nil
	}
	if length < -1 {
		return nil, fmt.Errorf("invalid bulk string length %d", length)
	}

	payload := make([]byte, length+2)
	if _, errRead := io.ReadFull(r, payload); errRead != nil {
		return nil, errRead
	}
	if payload[length] != '\r' || payload[length+1] != '\n' {
		return nil, fmt.Errorf("invalid bulk string terminator")
	}
	return payload[:length], nil
}

func readRESPArrayOfBulkStrings(r *bufio.Reader) ([][]byte, error) {
	prefix, errRead := r.ReadByte()
	if errRead != nil {
		return nil, errRead
	}
	if prefix != '*' {
		return nil, fmt.Errorf("expected array prefix '*', got %q", prefix)
	}

	line, errLine := readTestRESPLine(r)
	if errLine != nil {
		return nil, errLine
	}
	count, errParse := strconv.Atoi(line)
	if errParse != nil {
		return nil, fmt.Errorf("invalid array length %q: %v", line, errParse)
	}
	if count < 0 {
		return nil, fmt.Errorf("invalid array length %d", count)
	}

	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		item, errItem := readTestRESPBulkString(r)
		if errItem != nil {
			return nil, errItem
		}
		out = append(out, item)
	}
	return out, nil
}

func readTestRESPPubSubSubscribe(r *bufio.Reader) (string, int, error) {
	prefix, errRead := r.ReadByte()
	if errRead != nil {
		return "", 0, errRead
	}
	if prefix != '*' {
		return "", 0, fmt.Errorf("expected array prefix '*', got %q", prefix)
	}
	line, errLine := readTestRESPLine(r)
	if errLine != nil {
		return "", 0, errLine
	}
	count, errParse := strconv.Atoi(line)
	if errParse != nil {
		return "", 0, fmt.Errorf("invalid array length %q: %v", line, errParse)
	}
	if count != 3 {
		return "", 0, fmt.Errorf("subscribe ack length = %d, want 3", count)
	}
	kind, errKind := readTestRESPBulkString(r)
	if errKind != nil {
		return "", 0, errKind
	}
	if string(kind) != "subscribe" {
		return "", 0, fmt.Errorf("subscribe ack kind = %q", string(kind))
	}
	channel, errChannel := readTestRESPBulkString(r)
	if errChannel != nil {
		return "", 0, errChannel
	}
	prefix, errRead = r.ReadByte()
	if errRead != nil {
		return "", 0, errRead
	}
	if prefix != ':' {
		return "", 0, fmt.Errorf("expected integer prefix ':', got %q", prefix)
	}
	line, errLine = readTestRESPLine(r)
	if errLine != nil {
		return "", 0, errLine
	}
	subscriptions, errParse := strconv.Atoi(line)
	if errParse != nil {
		return "", 0, fmt.Errorf("invalid subscription count %q: %v", line, errParse)
	}
	return string(channel), subscriptions, nil
}

func readTestRESPPubSubMessage(r *bufio.Reader) (string, []byte, error) {
	items, errItems := readRESPArrayOfBulkStrings(r)
	if errItems != nil {
		return "", nil, errItems
	}
	if len(items) != 3 {
		return "", nil, fmt.Errorf("pubsub message length = %d, want 3", len(items))
	}
	if string(items[0]) != "message" {
		return "", nil, fmt.Errorf("pubsub message kind = %q", string(items[0]))
	}
	return string(items[1]), items[2], nil
}

func TestRedisQueueChannel(t *testing.T) {
	tests := []struct {
		name    string
		channel string
		want    string
	}{
		{name: "usage", channel: "usage", want: redisUsageChannel},
		{name: "usage mixed case", channel: " UsAgE ", want: redisUsageChannel},
		{name: "errors", channel: "errors", want: redisErrorsChannel},
		{name: "errors mixed case", channel: "\tERRORS\r\n", want: redisErrorsChannel},
		{name: "unsupported", channel: "error", want: ""},
		{name: "empty", channel: " ", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redisQueueChannel(tt.channel); got != tt.want {
				t.Fatalf("redisQueueChannel(%q) = %q, want %q", tt.channel, got, tt.want)
			}
		})
	}
}

func TestSubscribeRedisChannelMatchesTrimmedCaseInsensitiveChannel(t *testing.T) {
	messages, unsubscribe, ok := subscribeRedisChannel(" UsAgE ")
	if !ok {
		t.Fatalf("subscribeRedisChannel() ok = false, want true")
	}
	t.Cleanup(unsubscribe)

	select {
	case msg := <-messages:
		if string(msg) != `{"support_refresh":true}` {
			t.Fatalf("subscribeRedisChannel() initial message = %q", string(msg))
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for initial usage subscription message")
	}

	_, unsubscribeErrors, ok := subscribeRedisChannel("\tERRORS\r\n")
	if !ok {
		t.Fatalf("subscribeRedisChannel(errors) ok = false, want true")
	}
	t.Cleanup(unsubscribeErrors)
}

func TestPopRedisQueueItemsMatchesTrimmedCaseInsensitiveChannel(t *testing.T) {
	redisqueue.SetEnabled(false)
	redisqueue.SetEnabled(true)
	t.Cleanup(func() { redisqueue.SetEnabled(false) })

	redisqueue.Enqueue([]byte("mixed"))

	items, ok := popRedisQueueItems(" UsAgE ", 1)
	if !ok {
		t.Fatalf("popRedisQueueItems() ok = false, want true")
	}
	if len(items) != 1 || string(items[0]) != "mixed" {
		t.Fatalf("popRedisQueueItems() = %#v, want one mixed item", items)
	}

	if items, ok := popRedisQueueItems("\tERRORS\r\n", 1); ok || items != nil {
		t.Fatalf("popRedisQueueItems(errors) = %#v, %t; want nil, false", items, ok)
	}
}

func BenchmarkRedisQueueChannel(b *testing.B) {
	for b.Loop() {
		if got := redisQueueChannel(" UsAgE "); got != redisUsageChannel {
			b.Fatalf("redisQueueChannel() = %q", got)
		}
	}
}

func TestRedisProtocol_ManagementDisabled_RejectsConnection(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	redisqueue.SetEnabled(false)

	server := newTestServer(t)
	if server.managementRoutesEnabled.Load() {
		t.Fatalf("expected managementRoutesEnabled to be false")
	}

	addr, stop := startRedisMuxListener(t, server)
	t.Cleanup(stop)

	conn, errDial := net.DialTimeout("tcp", addr, time.Second)
	if errDial != nil {
		t.Fatalf("failed to dial redis listener: %v", errDial)
	}
	t.Cleanup(func() { _ = conn.Close() })

	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if errWrite := writeTestRESPCommand(conn, "PING"); errWrite != nil {
		t.Fatalf("failed to write RESP command: %v", errWrite)
	}

	buf := make([]byte, 1)
	_, errRead := conn.Read(buf)
	if errRead == nil {
		t.Fatalf("expected connection to be closed when management is disabled")
	}
	if ne, ok := errRead.(net.Error); ok && ne.Timeout() {
		t.Fatalf("expected connection to be closed when management is disabled, got timeout: %v", errRead)
	}
}

func TestMuxProtocol_IdleConnectionDoesNotBlockRedisConnection(t *testing.T) {
	const managementPassword = "test-management-password"

	t.Setenv("MANAGEMENT_PASSWORD", managementPassword)
	redisqueue.SetEnabled(false)
	t.Cleanup(func() { redisqueue.SetEnabled(false) })

	server := newTestServer(t)
	if !server.managementRoutesEnabled.Load() {
		t.Fatalf("expected managementRoutesEnabled to be true")
	}

	addr, stop := startRedisMuxListener(t, server)
	t.Cleanup(stop)

	idleConn, errDialIdle := net.DialTimeout("tcp", addr, time.Second)
	if errDialIdle != nil {
		t.Fatalf("failed to dial idle connection: %v", errDialIdle)
	}
	t.Cleanup(func() { _ = idleConn.Close() })

	conn, errDial := net.DialTimeout("tcp", addr, time.Second)
	if errDial != nil {
		t.Fatalf("failed to dial redis listener: %v", errDial)
	}
	t.Cleanup(func() { _ = conn.Close() })

	reader := bufio.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	if errWrite := writeTestRESPCommand(conn, "PING"); errWrite != nil {
		t.Fatalf("failed to write RESP command: %v", errWrite)
	}
	if msg, err := readTestRESPError(reader); err != nil {
		t.Fatalf("second connection was not served while idle connection was open: %v", err)
	} else if msg != "NOAUTH Authentication required." {
		t.Fatalf("unexpected PING response before AUTH: %q", msg)
	}
}

func TestRedisProtocol_HomeEnabled_DisablesConnection(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")
	redisqueue.SetEnabled(false)
	t.Cleanup(func() { redisqueue.SetEnabled(false) })

	server := newTestServer(t)
	if !server.managementRoutesEnabled.Load() {
		t.Fatalf("expected managementRoutesEnabled to be true")
	}
	if server.cfg == nil {
		t.Fatalf("expected server cfg to be non-nil")
	}
	server.cfg.Home.Enabled = true
	redisqueue.SetEnabled(true)

	addr, stop := startRedisMuxListener(t, server)
	t.Cleanup(stop)

	conn, errDial := net.DialTimeout("tcp", addr, time.Second)
	if errDial != nil {
		t.Fatalf("failed to dial redis listener: %v", errDial)
	}
	t.Cleanup(func() { _ = conn.Close() })

	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_ = writeTestRESPCommand(conn, "PING")

	if msg, err := readTestRESPError(bufio.NewReader(conn)); err != nil {
		t.Fatalf("failed to read home-mode RESP error: %v", err)
	} else if msg != "ERR redis usage output disabled in home mode" {
		t.Fatalf("unexpected disabled RESP error: %q", msg)
	}

	buf := make([]byte, 1)
	_, errRead := conn.Read(buf)
	if errRead == nil {
		t.Fatalf("expected connection to be closed after home-mode RESP error")
	}
	if ne, ok := errRead.(net.Error); ok && ne.Timeout() {
		t.Fatalf("expected connection to be closed after home-mode RESP error, got timeout: %v", errRead)
	}
}

func TestRedisProtocol_SUBSCRIBE_UsageSendsSupportRefresh(t *testing.T) {
	const managementPassword = "test-management-password"

	t.Setenv("MANAGEMENT_PASSWORD", managementPassword)
	redisqueue.SetEnabled(false)
	t.Cleanup(func() { redisqueue.SetEnabled(false) })

	server := newTestServer(t)
	if !server.managementRoutesEnabled.Load() {
		t.Fatalf("expected managementRoutesEnabled to be true")
	}

	addr, stop := startRedisMuxListener(t, server)
	t.Cleanup(stop)

	conn, errDial := net.DialTimeout("tcp", addr, time.Second)
	if errDial != nil {
		t.Fatalf("failed to dial redis listener: %v", errDial)
	}
	t.Cleanup(func() { _ = conn.Close() })

	reader := bufio.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if errWrite := writeTestRESPCommand(conn, "AUTH", managementPassword); errWrite != nil {
		t.Fatalf("failed to write AUTH command: %v", errWrite)
	}
	if msg, errRead := readTestRESPSimpleString(reader); errRead != nil {
		t.Fatalf("failed to read AUTH response: %v", errRead)
	} else if msg != "OK" {
		t.Fatalf("unexpected AUTH response: %q", msg)
	}

	if errWrite := writeTestRESPCommand(conn, "SUBSCRIBE", "usage"); errWrite != nil {
		t.Fatalf("failed to write SUBSCRIBE command: %v", errWrite)
	}
	channel, subscriptions, errSubscribe := readTestRESPPubSubSubscribe(reader)
	if errSubscribe != nil {
		t.Fatalf("failed to read subscribe response: %v", errSubscribe)
	}
	if channel != "usage" || subscriptions != 1 {
		t.Fatalf("unexpected subscribe response channel=%q subscriptions=%d", channel, subscriptions)
	}

	channel, payload, errMessage := readTestRESPPubSubMessage(reader)
	if errMessage != nil {
		t.Fatalf("failed to read support refresh message: %v", errMessage)
	}
	if channel != "usage" || string(payload) != `{"support_refresh":true}` {
		t.Fatalf("unexpected support refresh message channel=%q payload=%q", channel, string(payload))
	}

	redisqueue.Enqueue([]byte(`{"id":1}`))
	channel, payload, errMessage = readTestRESPPubSubMessage(reader)
	if errMessage != nil {
		t.Fatalf("failed to read usage message: %v", errMessage)
	}
	if channel != "usage" || string(payload) != `{"id":1}` {
		t.Fatalf("unexpected usage message channel=%q payload=%q", channel, string(payload))
	}
}

func TestRedisProtocol_SUBSCRIBE_ErrorsReceivesErrorEvents(t *testing.T) {
	const managementPassword = "test-management-password"

	t.Setenv("MANAGEMENT_PASSWORD", managementPassword)
	redisqueue.SetEnabled(false)
	t.Cleanup(func() { redisqueue.SetEnabled(false) })

	server := newTestServer(t)
	if !server.managementRoutesEnabled.Load() {
		t.Fatalf("expected managementRoutesEnabled to be true")
	}

	addr, stop := startRedisMuxListener(t, server)
	t.Cleanup(stop)

	conn, errDial := net.DialTimeout("tcp", addr, time.Second)
	if errDial != nil {
		t.Fatalf("failed to dial redis listener: %v", errDial)
	}
	t.Cleanup(func() { _ = conn.Close() })

	reader := bufio.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if errWrite := writeTestRESPCommand(conn, "AUTH", managementPassword); errWrite != nil {
		t.Fatalf("failed to write AUTH command: %v", errWrite)
	}
	if msg, errRead := readTestRESPSimpleString(reader); errRead != nil {
		t.Fatalf("failed to read AUTH response: %v", errRead)
	} else if msg != "OK" {
		t.Fatalf("unexpected AUTH response: %q", msg)
	}

	if errWrite := writeTestRESPCommand(conn, "SUBSCRIBE", "errors"); errWrite != nil {
		t.Fatalf("failed to write SUBSCRIBE command: %v", errWrite)
	}
	channel, subscriptions, errSubscribe := readTestRESPPubSubSubscribe(reader)
	if errSubscribe != nil {
		t.Fatalf("failed to read subscribe response: %v", errSubscribe)
	}
	if channel != "errors" || subscriptions != 1 {
		t.Fatalf("unexpected subscribe response channel=%q subscriptions=%d", channel, subscriptions)
	}

	redisqueue.EnqueueError([]byte(`{"auth_index":"auth-1","status_code":401}`))
	channel, payload, errMessage := readTestRESPPubSubMessage(reader)
	if errMessage != nil {
		t.Fatalf("failed to read error message: %v", errMessage)
	}
	if channel != "errors" || string(payload) != `{"auth_index":"auth-1","status_code":401}` {
		t.Fatalf("unexpected error message channel=%q payload=%q", channel, string(payload))
	}
}

func TestRedisProtocol_AUTH_And_PopContracts(t *testing.T) {
	const managementPassword = "test-management-password"

	t.Setenv("MANAGEMENT_PASSWORD", managementPassword)
	redisqueue.SetEnabled(false)
	t.Cleanup(func() { redisqueue.SetEnabled(false) })

	server := newTestServer(t)
	if !server.managementRoutesEnabled.Load() {
		t.Fatalf("expected managementRoutesEnabled to be true")
	}

	addr, stop := startRedisMuxListener(t, server)
	t.Cleanup(stop)

	conn, errDial := net.DialTimeout("tcp", addr, time.Second)
	if errDial != nil {
		t.Fatalf("failed to dial redis listener: %v", errDial)
	}
	t.Cleanup(func() { _ = conn.Close() })

	reader := bufio.NewReader(conn)

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if errWrite := writeTestRESPCommand(conn, "AUTH", managementPassword); errWrite != nil {
		t.Fatalf("failed to write AUTH command: %v", errWrite)
	}
	if msg, errRead := readTestRESPSimpleString(reader); errRead != nil {
		t.Fatalf("failed to read AUTH response: %v", errRead)
	} else if msg != "OK" {
		t.Fatalf("unexpected AUTH response: %q", msg)
	}

	if !redisqueue.Enabled() {
		t.Fatalf("expected redisqueue to be enabled")
	}
	redisqueue.Enqueue([]byte("a"))
	redisqueue.Enqueue([]byte("b"))
	redisqueue.Enqueue([]byte("c"))

	if errWrite := writeTestRESPCommand(conn, "RPOP", "usage"); errWrite != nil {
		t.Fatalf("failed to write RPOP command: %v", errWrite)
	}
	if item, errRead := readTestRESPBulkString(reader); errRead != nil {
		t.Fatalf("failed to read RPOP response: %v", errRead)
	} else if string(item) != "a" {
		t.Fatalf("unexpected RPOP item: %q", string(item))
	}

	if errWrite := writeTestRESPCommand(conn, "LPOP", "usage"); errWrite != nil {
		t.Fatalf("failed to write LPOP command: %v", errWrite)
	}
	if item, errRead := readTestRESPBulkString(reader); errRead != nil {
		t.Fatalf("failed to read LPOP response: %v", errRead)
	} else if string(item) != "b" {
		t.Fatalf("unexpected LPOP item: %q", string(item))
	}

	if errWrite := writeTestRESPCommand(conn, "RPOP", "usage", "10"); errWrite != nil {
		t.Fatalf("failed to write RPOP count command: %v", errWrite)
	}
	items, errItems := readRESPArrayOfBulkStrings(reader)
	if errItems != nil {
		t.Fatalf("failed to read RPOP count response: %v", errItems)
	}
	if len(items) != 1 || string(items[0]) != "c" {
		t.Fatalf("unexpected RPOP count items: %#v", items)
	}

	if errWrite := writeTestRESPCommand(conn, "LPOP", "usage"); errWrite != nil {
		t.Fatalf("failed to write LPOP empty command: %v", errWrite)
	}
	item, errItem := readTestRESPBulkString(reader)
	if errItem != nil {
		t.Fatalf("failed to read LPOP empty response: %v", errItem)
	}
	if item != nil {
		t.Fatalf("expected nil bulk string for empty queue, got %q", string(item))
	}

	if errWrite := writeTestRESPCommand(conn, "RPOP", "usage", "2"); errWrite != nil {
		t.Fatalf("failed to write RPOP empty count command: %v", errWrite)
	}
	emptyItems, errEmpty := readRESPArrayOfBulkStrings(reader)
	if errEmpty != nil {
		t.Fatalf("failed to read RPOP empty count response: %v", errEmpty)
	}
	if len(emptyItems) != 0 {
		t.Fatalf("expected empty array for empty queue with count, got %#v", emptyItems)
	}

	if errWrite := writeTestRESPCommand(conn, "RPOP", "errors", "2"); errWrite != nil {
		t.Fatalf("failed to write RPOP errors count command: %v", errWrite)
	}
	if msg, errRead := readTestRESPError(reader); errRead != nil {
		t.Fatalf("failed to read RPOP errors response: %v", errRead)
	} else if msg != "ERR unsupported channel 'errors'" {
		t.Fatalf("unexpected RPOP errors response: %q", msg)
	}
}

func TestReadRedisCommandRejectsOversizedRESP(t *testing.T) {
	t.Run("array length", func(t *testing.T) {
		reader := bufio.NewReader(strings.NewReader(fmt.Sprintf("*%d\r\n", maxRedisRESPArrayLength+1)))
		if _, err := readRedisCommand(reader); err == nil || !strings.Contains(err.Error(), "array length too large") {
			t.Fatalf("readRedisCommand() error = %v, want array length limit", err)
		}
	})

	t.Run("bulk length", func(t *testing.T) {
		reader := bufio.NewReader(strings.NewReader(fmt.Sprintf("*1\r\n$%d\r\n", maxRedisRESPBulkStringBytes+1)))
		if _, err := readRedisCommand(reader); err == nil || !strings.Contains(err.Error(), "bulk length too large") {
			t.Fatalf("readRedisCommand() error = %v, want bulk length limit", err)
		}
	})

	t.Run("line length", func(t *testing.T) {
		reader := bufio.NewReader(strings.NewReader("*" + strings.Repeat("1", maxRedisRESPLineBytes) + "\r\n"))
		if _, err := readRedisCommand(reader); err == nil || !strings.Contains(err.Error(), "line too long") {
			t.Fatalf("readRedisCommand() error = %v, want line length limit", err)
		}
	})
}
