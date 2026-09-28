package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

func dialHost(ctx context.Context, address, fingerprint string) (net.Conn, error) {
	config, err := pinnedTLS(fingerprint)
	if err != nil {
		return nil, err
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: handshakeTimeout}, Config: config}
	// DialContext completes certificate verification before returning. No
	// nickname or application data is sent until it succeeds.
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("could not connect to host: %w", err)
	}
	return connection, nil
}

func runClient(ctx context.Context, address, fingerprint, name string) error {
	connection, err := dialHost(ctx, address, fingerprint)
	if err != nil {
		return err
	}
	client := &peer{connection: connection, outgoing: make(chan outgoingMessage, outgoingCapacity), done: make(chan struct{})}
	stopOnCancel := context.AfterFunc(ctx, client.stop)
	defer stopOnCancel()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		client.writeMessages()
	}()
	defer func() {
		client.stop()
		<-writerDone
	}()
	client.send(message{Type: "hello", Name: name}, false)
	reader := newFrameReader(connection)
	connection.SetReadDeadline(time.Now().Add(helloTimeout))
	welcome, err := readMessage(reader)
	if err != nil {
		return fmt.Errorf("host disconnected before welcome (the room may be full): %w", err)
	}
	if welcome.Type == "error" {
		return errors.New(terminalText(welcome.Text))
	}
	if welcome.Type != "welcome" || welcome.Name != name {
		return errors.New("host sent an invalid welcome")
	}
	connection.SetReadDeadline(time.Time{})
	fmt.Printf("Connected as %s. Type /help for commands.\n", name)
	incoming := make(chan message)
	readErrors := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			msg, err := readMessage(reader)
			if err != nil {
				readErrors <- err
				return
			}
			select {
			case incoming <- msg:
			case <-client.done:
				return
			}
		}
	}()
	defer func() {
		client.stop()
		<-readerDone
	}()
	lines := make(chan inputLine)
	// Portable standard-library terminal reads cannot always be interrupted.
	// This one process-lifetime stdin goroutine may remain blocked until main
	// exits; it never owns a socket or prevents shutdown on host loss/Ctrl+C.
	go readTerminal(os.Stdin, lines, client.done)
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErrors:
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return errors.New("host disconnected; chat ended")
			}
			return fmt.Errorf("connection to host ended: %w", err)
		case msg := <-incoming:
			if err := printMessage(msg); err != nil {
				return err
			}
		case line, open := <-lines:
			if !open {
				return nil
			}
			if line.err != nil {
				fmt.Println("Error:", line.err)
				continue
			}
			switch strings.TrimSpace(line.text) {
			case "/quit":
				return nil
			case "/help":
				fmt.Println("Type a message and press Enter. /help shows help; /quit leaves. Messages: 1–2,000 UTF-8 bytes, not just whitespace.")
			default:
				if err := validateText(line.text); err != nil {
					fmt.Println("Error:", err)
					continue
				}
				if !client.send(message{Type: "chat", Text: line.text}, false) {
					return errors.New("connection to host is too slow or closed; chat ended")
				}
			}
		}
	}
}

type inputLine struct {
	text string
	err  error
}

func readTerminal(input io.Reader, lines chan<- inputLine, done <-chan struct{}) {
	defer close(lines)
	reader := bufio.NewReaderSize(input, maxTextBytes+2)
	for {
		line, err := reader.ReadSlice('\n')
		item := inputLine{text: strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")}
		if errors.Is(err, bufio.ErrBufferFull) {
			item = inputLine{err: errors.New("message exceeds 2,000 UTF-8 bytes")}
			// Drain this line without retaining it, then allow the next attempt.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = reader.ReadSlice('\n')
			}
		} else if err != nil && !errors.Is(err, io.EOF) {
			item = inputLine{err: fmt.Errorf("terminal input: %w", err)}
		}
		if len(line) > 0 || item.err != nil {
			select {
			case lines <- item:
			case <-done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func printMessage(msg message) error {
	switch msg.Type {
	case "chat":
		stamp, err := time.Parse(time.RFC3339, msg.Time)
		if err != nil {
			return errors.New("host sent an invalid timestamp")
		}
		fmt.Printf("[%s] %s: %s\n", stamp.Local().Format("15:04:05"), terminalText(msg.Name), terminalText(msg.Text))
	case "notice":
		fmt.Println("*", terminalText(msg.Text))
	case "error":
		fmt.Println("Error:", terminalText(msg.Text))
	default:
		return errors.New("host sent an unknown message type")
	}
	return nil
}
