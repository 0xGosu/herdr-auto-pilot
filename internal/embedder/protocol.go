package embedder

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// The embed-worker wire protocol. A parent Client and a child worker exchange
// one request/response per embed over the child's stdin/stdout. Framing is
// length-prefixed binary, NOT newline-delimited: the masked salient text
// carried in a request routinely contains newlines, so a line-oriented framing
// would corrupt it.
//
// Request  (parent → worker):  uint32 len (big-endian) + len bytes of UTF-8 text.
// Response (worker → parent):  1 status byte, then
//   - respOK:  uint32 dim count (big-endian) + dim × float32 (little-endian), or
//   - respErr: uint32 len (big-endian) + len bytes of UTF-8 error text.
//
// maxFrameBytes caps a single frame so a desynced pipe (e.g. a half-dead
// worker) can't make the peer allocate unbounded memory; salient text and a
// 384-dim vector are both far under it.
const (
	respOK  byte = 0
	respErr byte = 1

	maxFrameBytes = 8 << 20 // 8 MiB
)

// EmbedError is a well-formed error RESPONSE from a still-alive worker (a plain
// embed failure: missing model, bad input, worker-side degrade). The Client
// distinguishes it from a transport error — a dead worker / desynced pipe —
// because the former leaves the warm worker reusable while the latter forces a
// restart.
type EmbedError struct{ Msg string }

func (e *EmbedError) Error() string { return e.Msg }

// writeRequest frames text and writes it to w.
func writeRequest(w io.Writer, text string) error {
	return writeLenBytes(w, []byte(text))
}

// readRequest reads one framed request from r.
func readRequest(r io.Reader) (string, error) {
	b, err := readLenBytes(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// writeVecResponse writes a successful embedding response.
func writeVecResponse(w io.Writer, vec []float32) error {
	buf := make([]byte, 1+4+4*len(vec))
	buf[0] = respOK
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(vec)))
	for i, f := range vec {
		binary.LittleEndian.PutUint32(buf[5+4*i:9+4*i], math.Float32bits(f))
	}
	_, err := w.Write(buf)
	return err
}

// writeErrResponse writes an error response carrying msg.
func writeErrResponse(w io.Writer, msg string) error {
	b := []byte(msg)
	if len(b) > maxFrameBytes {
		b = b[:maxFrameBytes]
	}
	buf := make([]byte, 1+4+len(b))
	buf[0] = respErr
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(b)))
	copy(buf[5:], b)
	_, err := w.Write(buf)
	return err
}

// readResponse reads one response frame. A respErr frame is returned as a
// non-nil error whose text is the worker's message; a respOK frame yields the
// vector. An unexpected EOF (the worker died mid-response — e.g. a native
// SIGABRT) surfaces as an error here, which is exactly what turns an
// uncatchable native abort into a catchable Go error on the parent.
func readResponse(r io.Reader) ([]float32, error) {
	var status [1]byte
	if _, err := io.ReadFull(r, status[:]); err != nil {
		return nil, err
	}
	switch status[0] {
	case respErr:
		b, err := readLenBytes(r)
		if err != nil {
			return nil, err
		}
		return nil, &EmbedError{Msg: string(b)}
	case respOK:
		var lenBuf [4]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		if int64(n)*4 > maxFrameBytes {
			return nil, fmt.Errorf("embed response vector too large: %d dims", n)
		}
		raw := make([]byte, 4*n)
		if _, err := io.ReadFull(r, raw); err != nil {
			return nil, err
		}
		vec := make([]float32, n)
		for i := range vec {
			vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i : 4*i+4]))
		}
		return vec, nil
	default:
		return nil, fmt.Errorf("embed worker sent unknown response status %d", status[0])
	}
}

func writeLenBytes(w io.Writer, b []byte) error {
	if len(b) > maxFrameBytes {
		return fmt.Errorf("frame too large: %d bytes", len(b))
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readLenBytes(r io.Reader) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n > maxFrameBytes {
		return nil, fmt.Errorf("frame too large: %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}
