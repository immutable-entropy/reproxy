// Package websocket frames the WebSocket traffic the proxy carries, so each
// message can be recorded, shown and — when a plugin asks to — edited.
//
// After the HTTP upgrade the connection is a stream of frames in both
// directions (RFC 6455). This package reads that stream a logical message at
// a time: it reassembles the continuation frames of one message, undoes the
// permessage-deflate compression (RFC 7692) when it was negotiated, and
// leaves control frames (ping, pong, close) alone.
//
// It is Qt-free core and depends on nothing internal. The proxy drives it:
// Relay reads each side's frames, hands whole messages to a callback, and
// writes them on — the original bytes unchanged, or, when the callback edits
// a message, a fresh uncompressed frame. Editing is only offered on a
// connection whose compression the proxy stripped at the handshake (see
// Negotiation), so this package never has to compress.
package websocket

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"

	"reproxy/internal/randid"
)

// Direction is which way a message is going.
type Direction int

const (
	// ClientToServer is a message the browser sent.
	ClientToServer Direction = iota
	// ServerToClient is a message from the server.
	ServerToClient
)

func (d Direction) String() string {
	if d == ServerToClient {
		return "received"
	}
	return "sent"
}

// Type is a data message's kind.
type Type int

const (
	Text Type = iota
	Binary
)

func (t Type) String() string {
	if t == Binary {
		return "binary"
	}
	return "text"
}

// Opcodes (RFC 6455 §5.2).
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// Message is one reassembled data message.
type Message struct {
	Dir  Direction
	Type Type
	// Data is the message payload, decompressed. It may be truncated (see
	// Truncated): a message past the size limit is recorded only up to it.
	Data      []byte
	Truncated bool
}

// Decision is what a handler does with a message.
type Decision struct {
	// Data replaces the payload; nil leaves it unchanged. It is only
	// honoured on an editable connection (compression stripped).
	Data []byte
	// Drop discards the message instead of forwarding it.
	Drop bool
}

// Handler is called for each data message, after it is reassembled and
// decompressed, on the goroutine reading that direction. It may block (a
// plugin deciding), bounded by nothing here — the proxy bounds it. Returning
// the zero Decision forwards the message unchanged.
type Handler func(Message) Decision

// errMessageTooLarge marks a message past the size limit; it is handled
// (truncated for recording, forwarded whole) rather than failing the relay.
var errMessageTooLarge = errors.New("websocket: message past the size limit")

// Relay carries one direction of a WebSocket connection from src to dst,
// calling handle for each data message. maxMessage caps how much of a
// message is reassembled for the handler; the bytes still flow on.
// editable says the connection's compression was stripped, so an edited
// message can be re-framed uncompressed.
//
// It returns when src is exhausted or a write to dst fails.
func Relay(src *bufio.Reader, dst io.Writer, dir Direction, editable bool, maxMessage int, handle Handler) error {
	return RelayWith(src, dst, dir, editable, true, true, maxMessage, handle)
}

// RelayWith is Relay with the compression state spelled out: compressed says
// the connection negotiated permessage-deflate (so frames may carry
// compressed messages), contextTakeover whether the window is shared across
// them.
func RelayWith(src *bufio.Reader, dst io.Writer, dir Direction, editable, compressed, contextTakeover bool, maxMessage int, handle Handler) error {
	_ = compressed
	var inflater *inflater
	r := &reader{src: src, max: maxMessage}
	for {
		msg, frames, err := r.next()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		// Control frames and fragments we don't reassemble: forward as read.
		if msg == nil {
			if _, err := dst.Write(frames); err != nil {
				return err
			}
			continue
		}

		payload := msg.payload
		if msg.compressed {
			if inflater == nil {
				inflater = newInflater(contextTakeover)
			}
			data, err := inflater.inflate(payload, msg.truncated, maxMessage)
			if err != nil {
				// A bad compressed frame: forward the original and stop
				// decoding this side rather than killing the connection.
				if _, werr := dst.Write(frames); werr != nil {
					return werr
				}
				continue
			}
			payload = data
		}

		d := handle(Message{Dir: dir, Type: msg.typ, Data: payload, Truncated: msg.truncated})
		switch {
		case d.Drop:
			// Forward nothing.
		case editable && d.Data != nil:
			if _, err := dst.Write(reframe(dir, msg.typ, d.Data)); err != nil {
				return err
			}
		case editable && msg.compressed:
			// Shouldn't happen (editable implies stripped), but re-frame
			// the decoded payload rather than send compressed bytes.
			if _, err := dst.Write(reframe(dir, msg.typ, payload)); err != nil {
				return err
			}
		default:
			if _, err := dst.Write(frames); err != nil {
				return err
			}
		}
	}
}

// dataMessage is a reassembled data message with its framing kept.
type dataMessage struct {
	typ        Type
	payload    []byte
	compressed bool
	truncated  bool
}

// reader reassembles frames into messages. One message may span a first
// frame plus continuation frames; control frames may be interleaved.
type reader struct {
	src *bufio.Reader
	max int
}

// next returns the next data message with the raw bytes of every frame that
// made it, or, for a control frame or an oversize passthrough, a nil message
// and the raw bytes to forward.
func (r *reader) next() (*dataMessage, []byte, error) {
	var (
		raw        []byte
		payload    []byte
		typ        Type
		compressed bool
		truncated  bool
		assembling bool
	)
	for {
		f, err := readFrame(r.src)
		if err != nil {
			return nil, nil, err
		}
		raw = append(raw, f.raw...)

		if f.opcode >= opClose { // control frame: never fragmented
			if assembling {
				// A control frame mid-message: hand back what we have so
				// far as passthrough, keeping correctness over reassembly.
				return nil, raw, nil
			}
			return nil, raw, nil
		}

		switch f.opcode {
		case opText, opBinary:
			if assembling {
				return nil, raw, nil // protocol error; pass through
			}
			assembling = true
			typ = Text
			if f.opcode == opBinary {
				typ = Binary
			}
			compressed = f.rsv1
		case opContinuation:
			if !assembling {
				return nil, raw, nil
			}
		}

		if len(payload) < r.max {
			room := r.max - len(payload)
			if len(f.payload) > room {
				payload = append(payload, f.payload[:room]...)
				truncated = true
			} else {
				payload = append(payload, f.payload...)
			}
		} else if len(f.payload) > 0 {
			truncated = true
		}

		if f.fin {
			return &dataMessage{typ: typ, payload: payload, compressed: compressed, truncated: truncated}, raw, nil
		}
	}
}

// frame is one WebSocket frame.
type frame struct {
	fin     bool
	rsv1    bool
	opcode  byte
	payload []byte
	raw     []byte // the whole frame as read, for forwarding verbatim
}

// readFrame reads one frame. maxFramePayload bounds a single frame so a
// hostile length can't allocate without limit; a larger frame fails.
const maxFramePayload = 64 << 20

func readFrame(r *bufio.Reader) (frame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return frame{}, err
	}
	f := frame{
		fin:    hdr[0]&0x80 != 0,
		rsv1:   hdr[0]&0x40 != 0,
		opcode: hdr[0] & 0x0F,
	}
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7F)

	raw := []byte{hdr[0], hdr[1]}
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return frame{}, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
		raw = append(raw, ext[:]...)
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return frame{}, err
		}
		length = binary.BigEndian.Uint64(ext[:])
		raw = append(raw, ext[:]...)
	}
	if length > maxFramePayload {
		return frame{}, errMessageTooLarge
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return frame{}, err
		}
		raw = append(raw, mask[:]...)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return frame{}, err
	}
	raw = append(raw, payload...)
	if masked {
		unmask(payload, mask)
	}
	f.payload = payload
	f.raw = raw
	return f, nil
}

func unmask(b []byte, mask [4]byte) {
	for i := range b {
		b[i] ^= mask[i&3]
	}
}

// reframe writes a single uncompressed, final frame for an edited message.
// A client→server message must be masked (RFC 6455 §5.3); server→client
// must not be.
func reframe(dir Direction, typ Type, data []byte) []byte {
	if dir == ClientToServer {
		var mask [4]byte
		randid.Fill(mask[:])
		return frameBytes(typ, data, mask[:])
	}
	return frameBytes(typ, data, nil)
}

func frameBytes(typ Type, data []byte, mask []byte) []byte {
	op := byte(opText)
	if typ == Binary {
		op = opBinary
	}
	out := []byte{0x80 | op} // FIN, no RSV, data opcode
	n := len(data)
	maskBit := byte(0)
	if mask != nil {
		maskBit = 0x80
	}
	switch {
	case n < 126:
		out = append(out, maskBit|byte(n))
	case n < 1<<16:
		out = append(out, maskBit|126)
		out = binary.BigEndian.AppendUint16(out, uint16(n))
	default:
		out = append(out, maskBit|127)
		out = binary.BigEndian.AppendUint64(out, uint64(n))
	}
	if mask != nil {
		out = append(out, mask...)
		masked := make([]byte, n)
		copy(masked, data)
		var m [4]byte
		copy(m[:], mask)
		unmask(masked, m)
		return append(out, masked...)
	}
	return append(out, data...)
}
