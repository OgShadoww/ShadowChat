package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxFrameBytes = 16 * 1024 // Includes the terminating newline.
	maxTextBytes  = 2000
)

type message struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	Text string `json:"text,omitempty"`
	Time string `json:"time,omitempty"`
}

// A fixed-size reader bounds memory before parsing. ReadSlice handles TCP
// fragmentation and keeps any subsequent frames buffered for the next call.
func newFrameReader(reader io.Reader) *bufio.Reader {
	return bufio.NewReaderSize(reader, maxFrameBytes)
}

func readMessage(reader *bufio.Reader) (message, error) {
	var msg message
	frame, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(frame) > maxFrameBytes {
		return msg, errors.New("wire frame exceeds 16 KiB")
	}
	if err != nil {
		if errors.Is(err, io.EOF) && len(frame) != 0 {
			return msg, errors.New("incomplete JSON frame (missing newline)")
		}
		return msg, err
	}
	trimmed := strings.TrimSpace(string(frame))
	if !utf8.Valid(frame) || !strings.HasPrefix(trimmed, "{") || json.Unmarshal(frame, &msg) != nil {
		return msg, errors.New("malformed JSON object")
	}
	return msg, nil
}

func validateName(name string) error {
	if len(name) < 1 || len(name) > 24 {
		return errors.New("nickname must contain 1–24 ASCII letters, digits, underscores, or hyphens")
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return errors.New("nickname may contain only ASCII letters, digits, underscores, and hyphens")
		}
	}
	return nil
}

func validateText(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("message cannot be empty")
	}
	if len(text) > maxTextBytes {
		return errors.New("message exceeds 2,000 UTF-8 bytes")
	}
	if !utf8.ValidString(text) {
		return errors.New("message must be valid UTF-8")
	}
	return nil
}

// Untrusted text must not execute terminal escape sequences or forge lines.
func terminalText(text string) string {
	return strings.Map(func(char rune) rune {
		if unicode.IsControl(char) || char == '\u2028' || char == '\u2029' {
			return ' '
		}
		return char
	}, text)
}
