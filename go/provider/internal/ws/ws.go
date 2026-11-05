// Package ws implements the bounded local RFC 6455 text subset used by Codex.
package ws

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/llcoolkm/rookery/go/platform"
)

const MaxMessage = 16 << 20

var ErrProtocol = errors.New("invalid provider transport frame")

type Conn struct {
	conn   platform.Connection
	reader *bufio.Reader
}

func Open(conn platform.Connection) (*Conn, error) {
	c := &Conn{conn: conn, reader: bufio.NewReaderSize(conn, 16<<10)}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, ErrProtocol
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	if err := writeAll(conn, []byte("GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: "+key+"\r\n\r\n")); err != nil {
		return nil, ErrProtocol
	}
	headers := map[string]string{}
	total := 0
	rawLine, err := c.reader.ReadSlice('\n')
	line := string(rawLine)
	total += len(line)
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 101 ") {
		return nil, ErrProtocol
	}
	for {
		rawLine, err = c.reader.ReadSlice('\n')
		line = string(rawLine)
		total += len(line)
		if err != nil || total > 16<<10 {
			return nil, ErrProtocol
		}
		if line == "\r\n" {
			break
		}
		if !strings.HasSuffix(line, "\r\n") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			return nil, ErrProtocol
		}
		name, value, ok := strings.Cut(strings.TrimSuffix(line, "\r\n"), ":")
		name = strings.ToLower(strings.TrimSpace(name))
		if !ok || name == "" {
			return nil, ErrProtocol
		}
		if _, ok = headers[name]; ok {
			return nil, ErrProtocol
		}
		headers[name] = strings.TrimSpace(value)
	}
	hash := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	upgrade := false
	for _, v := range strings.Split(headers["connection"], ",") {
		if strings.EqualFold(strings.TrimSpace(v), "upgrade") {
			upgrade = true
		}
	}
	if !strings.EqualFold(headers["upgrade"], "websocket") || !upgrade || headers["sec-websocket-accept"] != base64.StdEncoding.EncodeToString(hash[:]) {
		return nil, ErrProtocol
	}
	if _, ok := headers["sec-websocket-extensions"]; ok {
		return nil, ErrProtocol
	}
	if _, ok := headers["sec-websocket-protocol"]; ok {
		return nil, ErrProtocol
	}
	return c, nil
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func (c *Conn) frame(op byte, p []byte) error {
	if len(p) > MaxMessage {
		return ErrProtocol
	}
	h := []byte{0x80 | op}
	switch {
	case len(p) < 126:
		h = append(h, 0x80|byte(len(p)))
	case len(p) < 65536:
		h = append(h, 0xfe, byte(len(p)>>8), byte(len(p)))
	default:
		h = append(h, 0xff)
		h = binary.BigEndian.AppendUint64(h, uint64(len(p)))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return ErrProtocol
	}
	h = append(h, mask[:]...)
	for i, b := range p {
		h = append(h, b^mask[i%4])
	}
	return writeAll(c.conn, h)
}
func (c *Conn) Send(p []byte) error { return c.frame(1, p) }
func (c *Conn) Receive() ([]byte, error) {
	var result []byte
	fragmented := false
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.reader, h[:]); err != nil {
			return nil, ErrProtocol
		}
		fin := h[0]&0x80 != 0
		op := h[0] & 15
		n := uint64(h[1] & 127)
		if h[0]&0x70 != 0 || h[1]&0x80 != 0 {
			return nil, ErrProtocol
		}
		if op != 0 && op != 1 && op != 8 && op != 9 && op != 10 {
			return nil, ErrProtocol
		}
		if op >= 8 && (!fin || n > 125) {
			return nil, ErrProtocol
		}
		if n == 126 {
			var b [2]byte
			if _, e := io.ReadFull(c.reader, b[:]); e != nil {
				return nil, ErrProtocol
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
			if n < 126 {
				return nil, ErrProtocol
			}
		} else if n == 127 {
			var b [8]byte
			if _, e := io.ReadFull(c.reader, b[:]); e != nil {
				return nil, ErrProtocol
			}
			n = binary.BigEndian.Uint64(b[:])
			if n < 65536 || n>>63 != 0 {
				return nil, ErrProtocol
			}
		}
		if n > MaxMessage || (op < 8 && uint64(len(result))+n > MaxMessage) {
			return nil, ErrProtocol
		}
		if op == 0 && !fragmented || op == 1 && fragmented {
			return nil, ErrProtocol
		}
		p := make([]byte, int(n))
		if _, err := io.ReadFull(c.reader, p); err != nil {
			return nil, ErrProtocol
		}
		switch op {
		case 8:
			return nil, ErrProtocol
		case 9:
			if err := c.frame(10, p); err != nil {
				return nil, ErrProtocol
			}
			continue
		case 10:
			continue
		}
		result = append(result, p...)
		if fin {
			if !utf8.Valid(result) {
				return nil, ErrProtocol
			}
			return result, nil
		}
		fragmented = true
	}
}
