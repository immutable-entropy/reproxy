package websocket

import (
	"bufio"
	"bytes"
	"compress/flate"
	"testing"
)

// clientFrame builds a masked data frame, as a browser sends.
func clientFrame(typ Type, data []byte) []byte {
	return frameBytes(typ, data, []byte{0xAA, 0xBB, 0xCC, 0xDD})
}

// serverFrame builds an unmasked data frame, as a server sends.
func serverFrame(typ Type, data []byte) []byte { return frameBytes(typ, data, nil) }

func TestRelayRecordsAndForwardsVerbatim(t *testing.T) {
	in := bytes.Join([][]byte{
		serverFrame(Text, []byte("hello")),
		serverFrame(Binary, []byte{1, 2, 3}),
		{0x89, 0x00}, // a ping: control frame, forwarded, not a message
	}, nil)
	var out bytes.Buffer
	var got []Message
	err := Relay(bufio.NewReader(bytes.NewReader(in)), &out, ServerToClient, false, 1<<20,
		func(m Message) Decision { got = append(got, m); return Decision{} })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got[0].Data) != "hello" || got[0].Type != Text || got[1].Type != Binary {
		t.Fatalf("messages = %+v", got)
	}
	if !bytes.Equal(out.Bytes(), in) {
		t.Fatal("non-editable relay changed the bytes")
	}
}

func TestRelayReassemblesFragments(t *testing.T) {
	// "hel" (fin=0, text) + "lo" (fin=0, continuation) + "!" (fin=1).
	frames := []byte{0x01, 0x03, 'h', 'e', 'l', 0x00, 0x02, 'l', 'o', 0x80, 0x01, '!'}
	var got []Message
	err := Relay(bufio.NewReader(bytes.NewReader(frames)), &bytes.Buffer{}, ServerToClient, false, 1<<20,
		func(m Message) Decision { got = append(got, m); return Decision{} })
	if err != nil || len(got) != 1 || string(got[0].Data) != "hello!" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestRelayEdits(t *testing.T) {
	in := clientFrame(Text, []byte("ping"))
	var out bytes.Buffer
	err := Relay(bufio.NewReader(bytes.NewReader(in)), &out, ClientToServer, true, 1<<20,
		func(m Message) Decision {
			if string(m.Data) != "ping" {
				t.Fatalf("saw %q", m.Data)
			}
			return Decision{Data: []byte("PONG!")}
		})
	if err != nil {
		t.Fatal(err)
	}
	// The forwarded frame is masked (client→server) and carries the edit.
	fr, rerr := readFrame(bufio.NewReader(bytes.NewReader(out.Bytes())))
	if rerr != nil || string(fr.payload) != "PONG!" {
		t.Fatalf("reframed = %q, %v", fr.payload, rerr)
	}
	if out.Bytes()[1]&0x80 == 0 {
		t.Fatal("client→server edit was not masked")
	}
}

func TestRelayDrops(t *testing.T) {
	in := serverFrame(Text, []byte("secret"))
	var out bytes.Buffer
	err := Relay(bufio.NewReader(bytes.NewReader(in)), &out, ServerToClient, true, 1<<20,
		func(m Message) Decision { return Decision{Drop: true} })
	if err != nil || out.Len() != 0 {
		t.Fatalf("dropped message forwarded %d bytes, %v", out.Len(), err)
	}
}

// compressedFrame builds a server permessage-deflate text frame for data,
// against the running flate writer (context takeover).
func deflateMessage(w *flate.Writer, buf *bytes.Buffer, data []byte) []byte {
	buf.Reset()
	w.Write(data)
	w.Flush()
	b := buf.Bytes()
	// Strip the trailing 00 00 ff ff sync marker.
	b = b[:len(b)-4]
	frame := []byte{0xC1} // FIN, RSV1 (compressed), text
	frame = append(frame, byte(len(b)))
	return append(frame, b...)
}

func TestRelayInflatesCompressedMessages(t *testing.T) {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	in := bytes.Join([][]byte{
		deflateMessage(w, &buf, []byte("the quick brown fox")),
		deflateMessage(w, &buf, []byte("the quick brown dog")), // shares the window
	}, nil)
	var got []string
	err := RelayWith(bufio.NewReader(bytes.NewReader(in)), &bytes.Buffer{}, ServerToClient, false, true, true, 1<<20,
		func(m Message) Decision { got = append(got, string(m.Data)); return Decision{} })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "the quick brown fox" || got[1] != "the quick brown dog" {
		t.Fatalf("inflated = %q", got)
	}
}

func TestStripExtensions(t *testing.T) {
	head := []byte("GET /ws HTTP/1.1\r\nHost: app.test\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\n\r\n")
	out := StripExtensions(head)
	if bytes.Contains(bytes.ToLower(out), []byte("sec-websocket-extensions")) {
		t.Fatalf("extension not stripped: %q", out)
	}
	if !bytes.Contains(out, []byte("Upgrade: websocket")) {
		t.Fatal("stripping removed other headers")
	}
}
