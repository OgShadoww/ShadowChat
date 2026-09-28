package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const (
	maxClients       = 32
	outgoingCapacity = 16
	handshakeTimeout = 5 * time.Second
	helloTimeout     = 5 * time.Second
	writeTimeout     = 5 * time.Second
)

type outgoingMessage struct {
	message message
	final   bool
}

type peer struct {
	connection net.Conn
	name       string
	outgoing   chan outgoingMessage
	done       chan struct{}
	closeOnce  sync.Once
}

// stop may be called by either connection goroutine or the room. Queues are
// never closed: done signals shutdown without racing a concurrent sender.
func (client *peer) stop() {
	client.closeOnce.Do(func() {
		close(client.done)
		if connection, ok := client.connection.(*tls.Conn); ok {
			// Close the transport directly: TLS close_notify can itself block on
			// a slow peer. Room eviction must not wait on any network write.
			connection.NetConn().Close()
		} else {
			client.connection.Close()
		}
	})
}

func (client *peer) send(msg message, final bool) bool {
	select {
	case <-client.done:
		return false
	default:
	}
	select {
	case client.outgoing <- outgoingMessage{message: msg, final: final}:
		return true
	default:
		client.stop() // A full queue must never stall the room.
		return false
	}
}

func (client *peer) writeMessages() {
	defer client.stop()
	encoder := json.NewEncoder(client.connection)
	for {
		select {
		case <-client.done:
			return
		case item := <-client.outgoing:
			if client.connection.SetWriteDeadline(time.Now().Add(writeTimeout)) != nil {
				return
			}
			if encoder.Encode(item.message) != nil || item.final {
				return
			}
		}
	}
}

func (client *peer) reject(text string) {
	client.send(message{Type: "error", Text: text}, true)
	// Let the sole writer deliver the error before closing; its deadline bounds
	// this wait even when the other side has stopped reading.
	<-client.done
}

type roomEvent struct {
	kind   string
	client *peer
	text   string
	reply  chan string
}

type server struct {
	listener net.Listener
	tls      *tls.Config
	context  context.Context
	cancel   context.CancelFunc
	events   chan roomEvent
	done     chan struct{}
}

func startServer(address string) (*server, string, error) {
	config, fingerprint, err := newHostTLS()
	if err != nil {
		return nil, "", err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	host := &server{
		listener: listener, tls: config, context: ctx, cancel: cancel,
		events: make(chan roomEvent), done: make(chan struct{}),
	}
	go host.serve()
	return host, fingerprint, nil
}

func (host *server) close() {
	host.cancel()
	<-host.done
}

func (host *server) deliver(event roomEvent) bool {
	select {
	case host.events <- event:
		return true
	case <-host.context.Done():
		return false
	}
}

func (host *server) serve() {
	defer close(host.done)
	stopListener := context.AfterFunc(host.context, func() { host.listener.Close() })
	defer stopListener()
	roomDone := make(chan struct{})
	go func() {
		defer close(roomDone)
		host.runRoom()
	}()
	var readers sync.WaitGroup
	// Pending TLS/hello connections also consume slots, bounding goroutines
	// even if a network peer connects without completing the protocol.
	slots := make(chan struct{}, maxClients)
	for {
		connection, err := host.listener.Accept()
		if err != nil {
			break
		}
		select {
		case slots <- struct{}{}:
			readers.Add(1)
			go func() {
				defer readers.Done()
				defer func() { <-slots }()
				host.readClient(connection)
			}()
		default:
			connection.Close()
		}
	}
	host.cancel()
	host.listener.Close()
	readers.Wait()
	<-roomDone
}

func (host *server) readClient(raw net.Conn) {
	connection := tls.Server(raw, host.tls)
	client := &peer{connection: connection, outgoing: make(chan outgoingMessage, outgoingCapacity), done: make(chan struct{})}
	stopOnShutdown := context.AfterFunc(host.context, client.stop)
	defer stopOnShutdown()
	defer client.stop()
	connection.SetDeadline(time.Now().Add(handshakeTimeout))
	if connection.HandshakeContext(host.context) != nil {
		return
	}
	connection.SetDeadline(time.Time{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		client.writeMessages()
	}()
	defer func() {
		client.stop()
		<-writerDone
	}()
	reader := newFrameReader(connection)
	connection.SetReadDeadline(time.Now().Add(helloTimeout))
	hello, err := readMessage(reader)
	if err != nil {
		client.reject("expected hello: " + err.Error())
		return
	}
	if hello.Type != "hello" {
		client.reject("first message must be hello")
		return
	}
	if err := validateName(hello.Name); err != nil {
		client.reject(err.Error())
		return
	}
	client.name = hello.Name
	reply := make(chan string, 1)
	if !host.deliver(roomEvent{kind: "join", client: client, reply: reply}) {
		return
	}
	// Every submitted join has a matching leave, including shutdown races.
	defer host.deliver(roomEvent{kind: "leave", client: client})
	select {
	case reason := <-reply:
		if reason != "" {
			client.reject(reason)
			return
		}
	case <-host.context.Done():
		return
	}
	connection.SetReadDeadline(time.Time{}) // Silence is healthy after hello.
	for {
		msg, err := readMessage(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				client.reject(err.Error())
			}
			return
		}
		if msg.Type != "chat" {
			client.send(message{Type: "error", Text: "unknown message type; expected chat"}, false)
			continue
		}
		if err := validateText(msg.Text); err != nil {
			client.send(message{Type: "error", Text: err.Error()}, false)
			continue
		}
		// Ignore any name or timestamp supplied by the client.
		if !host.deliver(roomEvent{kind: "chat", client: client, text: msg.Text}) {
			return
		}
	}
}

func (host *server) runRoom() {
	// Only this goroutine reads or changes room membership.
	clients := make(map[string]*peer)
	for {
		select {
		case <-host.context.Done():
			for _, client := range clients {
				client.stop()
			}
			return
		case event := <-host.events:
			client := event.client
			switch event.kind {
			case "join":
				if _, exists := clients[client.name]; exists {
					event.reply <- "Nickname already in use"
					continue
				}
				if len(clients) >= maxClients {
					event.reply <- "Room is full (32 clients)"
					continue
				}
				clients[client.name] = client
				client.send(message{Type: "welcome", Name: client.name}, false)
				broadcast(clients, message{Type: "notice", Text: client.name + " joined"})
				event.reply <- ""
			case "leave":
				if clients[client.name] == client {
					delete(clients, client.name)
					client.stop()
					broadcast(clients, message{Type: "notice", Text: client.name + " left"})
				}
			case "chat":
				if clients[client.name] == client {
					broadcast(clients, message{Type: "chat", Name: client.name, Text: event.text, Time: time.Now().UTC().Format(time.RFC3339)})
				}
			}
		}
	}
}

func broadcast(clients map[string]*peer, msg message) {
	pending := []message{msg}
	for len(pending) > 0 {
		next := pending[0]
		pending = pending[1:]
		for name, client := range clients {
			if !client.send(next, false) {
				delete(clients, name)
				pending = append(pending, message{Type: "notice", Text: name + " left"})
			}
		}
	}
}
