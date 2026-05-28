package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type Encoder struct {
	w io.Writer
}

func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

func (e *Encoder) Encode(msg interface{}) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	line := escapeLineTerminators(string(data)) + "\n"
	_, err = io.WriteString(e.w, line)
	return err
}

type Decoder struct {
	scanner *bufio.Scanner
}

func NewDecoder(r io.Reader) *Decoder {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	return &Decoder{scanner: scanner}
}

func (d *Decoder) Decode() (json.RawMessage, error) {
	for d.scanner.Scan() {
		line := d.scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		msg := make([]byte, len(line))
		copy(msg, line)
		return json.RawMessage(msg), nil
	}
	if err := d.scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stdout: %w", err)
	}
	return nil, io.EOF
}

func ParseType(raw json.RawMessage) (msgType, subtype string, err error) {
	var header RawMessage
	if err := json.Unmarshal(raw, &header); err != nil {
		return "", "", fmt.Errorf("parse message type: %w", err)
	}
	return header.Type, header.Subtype, nil
}

func escapeLineTerminators(s string) string {
	s = strings.ReplaceAll(s, " ", "\\u2028")
	s = strings.ReplaceAll(s, " ", "\\u2029")
	return s
}
