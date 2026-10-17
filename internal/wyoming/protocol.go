package wyoming

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
)

// Reader wraps a net.Conn and reads Wyoming protocol messages.
//
// Both ReadHeader and ReadPayload go through the same bufio.Reader so that
// lookahead buffering for header lines doesn't silently consume binary PCM
// payload bytes that the next ReadPayload call expects to receive from the
// connection. Using bufio.Scanner + raw net.Conn reads was the original
// approach, but Scanner's internal read-ahead would buffer PCM bytes after
// the '\n', leaving ReadPayload to read stale data from the raw conn.
type Reader struct {
	r *bufio.Reader
}

// NewReader creates a Reader for conn.
func NewReader(conn net.Conn) *Reader {
	return &Reader{r: bufio.NewReader(conn)}
}

// ReadHeader reads the next Wyoming message header, plus any event data that
// follows it. Returns io.EOF when the connection is cleanly closed.
//
// Wyoming 1.9+ sends event data as a separate chunk of DataLength bytes
// immediately after the header \n, without a separating newline. This method
// reads that chunk when DataLength > 0 and stores the parsed JSON in h.Data,
// so callers see a uniform Header regardless of which wire format was used.
func (r *Reader) ReadHeader() (Header, error) {
	line, err := r.r.ReadString('\n')
	if err != nil {
		if err == io.EOF && len(line) == 0 {
			return Header{}, io.EOF
		}
		if err == io.EOF {
			// partial line without newline — treat as clean close
			return Header{}, io.EOF
		}
		return Header{}, fmt.Errorf("wyoming/protocol: read header: %w", err)
	}
	line = strings.TrimSuffix(line, "\n")

	var h Header
	if err := json.Unmarshal([]byte(line), &h); err != nil {
		return Header{}, fmt.Errorf("wyoming/protocol: parse header %q: %w", line, err)
	}

	// Wyoming 1.9+: event data follows the header as exactly DataLength bytes
	// with no intervening newline. Read it and merge into h.Data.
	if h.DataLength > 0 {
		dataBytes := make([]byte, h.DataLength)
		if _, err := io.ReadFull(r.r, dataBytes); err != nil {
			return Header{}, fmt.Errorf("wyoming/protocol: read event data (%d bytes): %w", h.DataLength, err)
		}
		var data interface{}
		if err := json.Unmarshal(dataBytes, &data); err != nil {
			return Header{}, fmt.Errorf("wyoming/protocol: parse event data %q: %w", dataBytes, err)
		}
		h.Data = data
	}

	return h, nil
}

// ReadPayload reads exactly n bytes of binary payload that immediately
// follows the header in the stream.
func (r *Reader) ReadPayload(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r.r, buf); err != nil {
		return nil, fmt.Errorf("wyoming/protocol: read payload (%d bytes): %w", n, err)
	}
	return buf, nil
}

// DecodeData unmarshals h.Data (which json.Unmarshal leaves as a
// map[string]interface{}) into dst.
func DecodeData(h Header, dst interface{}) error {
	raw, err := json.Marshal(h.Data)
	if err != nil {
		return fmt.Errorf("wyoming/protocol: re-marshal data: %w", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("wyoming/protocol: decode data for type %q: %w", h.Type, err)
	}
	return nil
}

// Writer wraps a net.Conn and writes Wyoming protocol messages.
type Writer struct {
	conn net.Conn
}

// NewWriter creates a Writer for conn.
func NewWriter(conn net.Conn) *Writer {
	return &Writer{conn: conn}
}

// wireHeader is the JSON structure written to the wire.
// We always emit Wyoming 1.9+ format: event data is sent separately via
// data_length rather than inlined in the "data" field.
type wireHeader struct {
	Type          string `json:"type"`
	DataLength    int    `json:"data_length,omitempty"`
	PayloadLength int    `json:"payload_length,omitempty"`
}

// WriteMessage writes a Wyoming 1.9 message: JSON header line, then event
// data bytes (if any), then binary payload bytes (if any).
func (w *Writer) WriteMessage(msgType string, data interface{}, payload []byte) error {
	var dataBytes []byte
	if data != nil {
		var err error
		dataBytes, err = json.Marshal(data)
		if err != nil {
			return fmt.Errorf("wyoming/protocol: marshal data for %s: %w", msgType, err)
		}
	}

	h := wireHeader{
		Type:          msgType,
		DataLength:    len(dataBytes),
		PayloadLength: len(payload),
	}
	hBytes, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("wyoming/protocol: marshal header for %s: %w", msgType, err)
	}
	hBytes = append(hBytes, '\n')

	if _, err := w.conn.Write(hBytes); err != nil {
		return fmt.Errorf("wyoming/protocol: write %s header: %w", msgType, err)
	}
	if len(dataBytes) > 0 {
		if _, err := w.conn.Write(dataBytes); err != nil {
			return fmt.Errorf("wyoming/protocol: write %s data: %w", msgType, err)
		}
	}
	if len(payload) > 0 {
		if _, err := w.conn.Write(payload); err != nil {
			return fmt.Errorf("wyoming/protocol: write %s payload: %w", msgType, err)
		}
	}
	return nil
}
