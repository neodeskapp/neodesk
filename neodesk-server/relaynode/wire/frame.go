// Package wire is the control protocol between the central server and its
// relay nodes: framing, message types, the enrollment string, and the key,
// certificate and TLS helpers both sides use.
//
// A frame is a 4-byte big-endian length followed by that many bytes of
// JSON, an object whose "t" field names the message type. A frame is at
// most MaxFrame bytes.
package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// MaxFrame is the largest frame body either side sends or accepts.
const MaxFrame = 1 << 20

// ErrFrameTooLarge is returned for a frame longer than MaxFrame, read or
// written. The reader does not read the body of such a frame.
var ErrFrameTooLarge = errors.New("wire: frame larger than 1 MiB")

// Message is a control message; Type is its "t" value.
type Message interface {
	Type() string
}

// Frame is a frame read from the wire: its type and the whole JSON body.
type Frame struct {
	Type string
	Body json.RawMessage
}

// Decode unmarshals the frame's body into v, a pointer to a message struct.
func (f Frame) Decode(v any) error {
	if err := json.Unmarshal(f.Body, v); err != nil {
		return fmt.Errorf("wire: decode %s: %w", f.Type, err)
	}
	return nil
}

// Encode returns the frame body of m: m's JSON with "t" first.
func Encode(m Message) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("wire: encode %s: %w", m.Type(), err)
	}
	if len(b) < 2 || b[0] != '{' {
		return nil, fmt.Errorf("wire: encode %s: not a JSON object", m.Type())
	}
	t, _ := json.Marshal(m.Type())
	var out bytes.Buffer
	out.Grow(len(b) + len(t) + 6)
	out.WriteString(`{"t":`)
	out.Write(t)
	if len(b) > 2 {
		out.WriteByte(',')
		out.Write(b[1:])
	} else {
		out.WriteByte('}')
	}
	return out.Bytes(), nil
}

// WriteFrame writes one frame holding m.
func WriteFrame(w io.Writer, m Message) error {
	body, err := Encode(m)
	if err != nil {
		return err
	}
	if len(body) > MaxFrame {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads one frame. A frame over MaxFrame, an empty frame and a
// body without a "t" are errors.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return Frame{}, ErrFrameTooLarge
	}
	if n == 0 {
		return Frame{}, errors.New("wire: empty frame")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Frame{}, err
	}
	var head struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return Frame{}, fmt.Errorf("wire: frame is not a JSON object: %w", err)
	}
	if head.T == "" {
		return Frame{}, errors.New("wire: frame without a type")
	}
	return Frame{Type: head.T, Body: body}, nil
}

// Conn is a control connection: reads with a deadline, and writes that
// any goroutine may send, one frame at a time.
type Conn struct {
	c   net.Conn
	wmu sync.Mutex
}

// NewConn wraps c.
func NewConn(c net.Conn) *Conn { return &Conn{c: c} }

// Net returns the underlying connection.
func (c *Conn) Net() net.Conn { return c.c }

// Read reads one frame, waiting at most timeout (0: no deadline).
func (c *Conn) Read(timeout time.Duration) (Frame, error) {
	if timeout > 0 {
		_ = c.c.SetReadDeadline(time.Now().Add(timeout))
	} else {
		_ = c.c.SetReadDeadline(time.Time{})
	}
	return ReadFrame(c.c)
}

// Write sends m, waiting at most timeout for the write (0: no deadline).
func (c *Conn) Write(m Message, timeout time.Duration) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if timeout > 0 {
		_ = c.c.SetWriteDeadline(time.Now().Add(timeout))
	} else {
		_ = c.c.SetWriteDeadline(time.Time{})
	}
	return WriteFrame(c.c, m)
}

// Close closes the connection.
func (c *Conn) Close() error { return c.c.Close() }
