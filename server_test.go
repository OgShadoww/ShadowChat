package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

const testTimeout = 3 * time.Second

type testClient struct {
	connection net.Conn
	reader     *bufio.Reader
}

func testHost(t *testing.T) (*server, string) {
	t.Helper()
	host, fingerprint, err := startServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.close)
	return host, fingerprint
}

func connectTestClient(t *testing.T, host *server, fingerprint string) *testClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	connection, err := dialHost(ctx, host.listener.Addr().String(), fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	return &testClient{connection: connection, reader: newFrameReader(connection)}
}

func joinTestClient(t *testing.T, host *server, fingerprint, name string) *testClient {
	t.Helper()
	client := connectTestClient(t, host, fingerprint)
	client.send(t, message{Type: "hello", Name: name})
	if msg := client.receive(t); msg.Type != "welcome" || msg.Name != name {
		t.Fatalf("expected welcome for %s, got %+v", name, msg)
	}
	client.expect(t, "notice", name+" joined")
	return client
}

func (client *testClient) send(t *testing.T, msg message) {
	t.Helper()
	client.connection.SetWriteDeadline(time.Now().Add(testTimeout))
	if err := json.NewEncoder(client.connection).Encode(msg); err != nil {
		t.Fatal(err)
	}
}

func (client *testClient) write(t *testing.T, frame string) {
	t.Helper()
	client.connection.SetWriteDeadline(time.Now().Add(testTimeout))
	if _, err := io.WriteString(client.connection, frame); err != nil {
		t.Fatal(err)
	}
}

func (client *testClient) receive(t *testing.T) message {
	t.Helper()
	client.connection.SetReadDeadline(time.Now().Add(testTimeout))
	msg, err := readMessage(client.reader)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func (client *testClient) expect(t *testing.T, kind, text string) message {
	t.Helper()
	msg := client.receive(t)
	if msg.Type != kind || msg.Text != text {
		t.Fatalf("expected %s %q, got %+v", kind, text, msg)
	}
	return msg
}

func TestTwoClientsExchangeMessage(t *testing.T) {
	host, fingerprint := testHost(t)
	orest := joinTestClient(t, host, fingerprint, "Orest")
	sophia := joinTestClient(t, host, fingerprint, "Sophia")
	orest.expect(t, "notice", "Sophia joined")
	// Supplied identity and time must not override connection identity/time.
	sophia.send(t, message{Type: "chat", Name: "Imposter", Text: "Hello!", Time: "fake"})
	for _, client := range []*testClient{orest, sophia} {
		msg := client.expect(t, "chat", "Hello!")
		if msg.Name != "Sophia" {
			t.Fatalf("untrusted sender accepted: %+v", msg)
		}
		if _, err := time.Parse(time.RFC3339, msg.Time); err != nil {
			t.Fatalf("invalid server timestamp: %v", err)
		}
	}
	orest.send(t, message{Type: "chat", Text: "Hi Sophia"})
	orest.expect(t, "chat", "Hi Sophia")
	sophia.expect(t, "chat", "Hi Sophia")
}

func TestDuplicateNickname(t *testing.T) {
	host, fingerprint := testHost(t)
	original := joinTestClient(t, host, fingerprint, "Orest")
	duplicate := connectTestClient(t, host, fingerprint)
	duplicate.send(t, message{Type: "hello", Name: "Orest"})
	duplicate.expect(t, "error", "Nickname already in use")
	if _, err := readMessage(duplicate.reader); err == nil {
		t.Fatal("rejected connection stayed open")
	}
	original.send(t, message{Type: "chat", Text: "Still here"})
	original.expect(t, "chat", "Still here")
}

func TestDisconnectKeepsRoomWorking(t *testing.T) {
	host, fingerprint := testHost(t)
	orest := joinTestClient(t, host, fingerprint, "Orest")
	sophia := joinTestClient(t, host, fingerprint, "Sophia")
	orest.expect(t, "notice", "Sophia joined")
	sophia.connection.Close()
	orest.expect(t, "notice", "Sophia left")
	orest.send(t, message{Type: "chat", Text: "Still chatting"})
	orest.expect(t, "chat", "Still chatting")
	joinTestClient(t, host, fingerprint, "Sophia") // Name is reusable after leave.
	orest.expect(t, "notice", "Sophia joined")
}

func TestSplitAndCombinedFrames(t *testing.T) {
	host, fingerprint := testHost(t)
	client := connectTestClient(t, host, fingerprint)
	client.write(t, `{"type":"hel`)
	client.write(t, "lo\",\"name\":\"Sophia\"}\n{\"type\":\"chat\",\"text\":\"one\"}\n{\"type\":\"chat\",\"text\":\"two\\nlines\"}\n")
	if msg := client.receive(t); msg.Type != "welcome" {
		t.Fatalf("expected welcome, got %+v", msg)
	}
	client.expect(t, "notice", "Sophia joined")
	client.expect(t, "chat", "one")
	client.expect(t, "chat", "two\nlines")
}

func TestBadFramesDoNotCrashServer(t *testing.T) {
	host, fingerprint := testHost(t)
	observer := joinTestClient(t, host, fingerprint, "Observer")
	for _, test := range []struct{ name, frame string }{
		{"Malformed", "{bad json}\n"},
		{"Oversized", strings.Repeat("x", maxFrameBytes+1) + "\n"},
		{"NotObject", "null\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := joinTestClient(t, host, fingerprint, test.name)
			observer.expect(t, "notice", test.name+" joined")
			client.write(t, test.frame)
			if msg := client.receive(t); msg.Type != "error" {
				t.Fatalf("expected error, got %+v", msg)
			}
			observer.expect(t, "notice", test.name+" left")
			observer.send(t, message{Type: "chat", Text: "Room is alive"})
			observer.expect(t, "chat", "Room is alive")
		})
	}
}

func TestIncorrectFingerprint(t *testing.T) {
	host, fingerprint := testHost(t)
	wrong := strings.Repeat("0", 64)
	if wrong == fingerprint {
		wrong = strings.Repeat("1", 64)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	connection, err := dialHost(ctx, host.listener.Addr().String(), wrong)
	if connection != nil {
		connection.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("expected fingerprint mismatch, got %v", err)
	}
	if _, err := pinnedTLS(""); err == nil {
		t.Fatal("missing fingerprint accepted")
	}
	joinTestClient(t, host, fingerprint, "CorrectPin")
}

func TestProtocolValidation(t *testing.T) {
	host, fingerprint := testHost(t)
	client := connectTestClient(t, host, fingerprint)
	client.send(t, message{Type: "chat", Text: "No hello"})
	client.expect(t, "error", "first message must be hello")
	client = joinTestClient(t, host, fingerprint, "Valid")
	for _, msg := range []message{
		{Type: "unknown"},
		{Type: "hello", Name: "AnotherName"},
		{Type: "chat", Text: "   "},
		{Type: "chat", Text: strings.Repeat("é", 1001)},
	} {
		client.send(t, msg)
		if reply := client.receive(t); reply.Type != "error" {
			t.Fatalf("expected error for %+v, got %+v", msg, reply)
		}
	}
	client.send(t, message{Type: "chat", Text: strings.Repeat("é", 1000)})
	client.expect(t, "chat", strings.Repeat("é", 1000))
	for _, name := range []string{"", "has space", "é", strings.Repeat("a", 25)} {
		if validateName(name) == nil {
			t.Errorf("accepted invalid name %q", name)
		}
	}
	if err := validateName("Orest_123-test"); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownClosesActiveAndPendingConnections(t *testing.T) {
	host, fingerprint := testHost(t)
	client := joinTestClient(t, host, fingerprint, "Orest")
	pending := connectTestClient(t, host, fingerprint) // Never sends hello.
	raw, err := net.DialTimeout("tcp", host.listener.Addr().String(), testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	closed := make(chan struct{})
	go func() {
		host.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(testTimeout):
		t.Fatal("server shutdown blocked")
	}
	for _, connection := range []net.Conn{client.connection, pending.connection, raw} {
		connection.SetReadDeadline(time.Now().Add(testTimeout))
		if _, err := connection.Read(make([]byte, 1)); err == nil {
			t.Fatal("connection survived shutdown")
		} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("connection timed out instead of closing")
		}
	}
}

func TestSlowClientDoesNotBlockBroadcast(t *testing.T) {
	slowConnection, slowRemote := net.Pipe()
	defer slowRemote.Close()
	fastConnection, fastRemote := net.Pipe()
	defer fastRemote.Close()
	slow := &peer{connection: slowConnection, name: "Slow", outgoing: make(chan outgoingMessage, 1), done: make(chan struct{})}
	fast := &peer{connection: fastConnection, name: "Fast", outgoing: make(chan outgoingMessage, 4), done: make(chan struct{})}
	defer slow.stop()
	defer fast.stop()
	slow.outgoing <- outgoingMessage{message: message{Type: "notice"}}
	clients := map[string]*peer{"Slow": slow, "Fast": fast}
	completed := make(chan struct{})
	go func() {
		broadcast(clients, message{Type: "chat", Text: "hello"})
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(testTimeout):
		t.Fatal("slow client blocked room")
	}
	if _, exists := clients["Slow"]; exists {
		t.Fatal("slow client not evicted")
	}
	if msg := <-fast.outgoing; msg.message.Text != "hello" {
		t.Fatalf("lost chat: %+v", msg)
	}
	if msg := <-fast.outgoing; msg.message.Text != "Slow left" {
		t.Fatalf("missing departure: %+v", msg)
	}
}

func TestTerminalInputRecoversAfterOversizedLine(t *testing.T) {
	lines := make(chan inputLine, 3)
	readTerminal(strings.NewReader(strings.Repeat("x", maxFrameBytes)+"\nhello\n"), lines, make(chan struct{}))
	if line := <-lines; line.err == nil {
		t.Fatal("oversized input accepted")
	}
	if line := <-lines; line.text != "hello" || line.err != nil {
		t.Fatalf("did not recover: %+v", line)
	}
	if got := terminalText("hello\x1b[2J\nforged"); got != "hello [2J forged" {
		t.Fatalf("unsafe terminal text %q", got)
	}
}
