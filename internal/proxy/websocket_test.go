package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/intercept"
	"reproxy/internal/websocket"
)

// maskedText builds a masked client text frame.
func maskedText(s string) []byte {
	mask := [4]byte{1, 2, 3, 4}
	out := []byte{0x81, 0x80 | byte(len(s))}
	out = append(out, mask[:]...)
	for i := 0; i < len(s); i++ {
		out = append(out, s[i]^mask[i&3])
	}
	return out
}

// serverText builds an unmasked server text frame.
func serverText(s string) []byte {
	return append([]byte{0x81, byte(len(s))}, s...)
}

// readServerText reads one unmasked server text frame's payload.
func readServerText(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		t.Fatal(err)
	}
	n := int(h[1] & 0x7F)
	buf := make([]byte, n)
	io.ReadFull(r, buf)
	return string(buf)
}

// wsBackend is a raw TCP server that completes a WebSocket upgrade, then
// reads one client frame and echoes a fixed reply, recording what it got.
func wsBackend(t *testing.T, got chan<- string) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"+
			"Connection: Upgrade\r\nSec-WebSocket-Accept: x\r\n\r\n")
		// Read one client frame (masked), unmask, report.
		var h [2]byte
		io.ReadFull(br, h[:])
		n := int(h[1] & 0x7F)
		var mask [4]byte
		io.ReadFull(br, mask[:])
		payload := make([]byte, n)
		io.ReadFull(br, payload)
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
		got <- string(payload)
		c.Write(serverText("world"))
		time.Sleep(50 * time.Millisecond)
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// editHandler is a test WSHandler that upcases client→server messages.
type editHandler struct{}

func (h *editHandler) Edits(intercept.Subject) bool { return true }
func (h *editHandler) Decide(_ context.Context, m proxy_WSInfo) websocket.Decision {
	if m.Dir == websocket.ClientToServer {
		return websocket.Decision{Data: []byte(strings.ToUpper(string(m.Data)))}
	}
	return websocket.Decision{}
}
func (h *editHandler) Opened(uint64, intercept.Subject) {}
func (h *editHandler) Closed(uint64)                    {}

type proxy_WSInfo = WSMessageInfo

func TestWebSocketRecordedAndEdited(t *testing.T) {
	gotBackend := make(chan string, 1)
	backend := wsBackend(t, gotBackend)

	store := capture.NewStore()
	srv := New(store, WithLogger(slog.New(slog.DiscardHandler)))
	srv.SetWebSocket(&editHandler{})
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, rep := socksDial(t, srv.Addr(), backend.Addr().String())
	if rep != repSucceeded {
		t.Fatalf("socks reply %d", rep)
	}
	br := bufio.NewReader(conn)
	io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: app.test\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZQ==\r\nSec-WebSocket-Version: 13\r\n"+
		"Sec-WebSocket-Extensions: permessage-deflate\r\n\r\n")
	// Read the 101.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	conn.Write(maskedText("hello"))
	if reply := readServerText(t, br); reply != "world" {
		t.Fatalf("browser got %q", reply)
	}

	if got := <-gotBackend; got != "HELLO" {
		t.Fatalf("backend got %q, want HELLO (the edit)", got)
	}
	waitFor(t, "two recorded messages", func() bool {
		for _, f := range store.List() {
			if len(f.Messages) >= 2 {
				return true
			}
		}
		return false
	})
	var msgs []capture.WSMessage
	for _, f := range store.List() {
		if len(f.Messages) > 0 {
			msgs = f.Messages
		}
	}
	if msgs[0].Dir != capture.Sent || string(msgs[0].Data) != "HELLO" || !msgs[0].Edited || string(msgs[0].Original) != "hello" {
		t.Fatalf("sent message = %+v", msgs[0])
	}
	if msgs[1].Dir != capture.Received || string(msgs[1].Data) != "world" {
		t.Fatalf("received message = %+v", msgs[1])
	}
}

var _ = binary.BigEndian
