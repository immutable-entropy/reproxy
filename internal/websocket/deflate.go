package websocket

import (
	"bytes"
	"compress/flate"
	"io"
	"strings"
)

// permessage-deflate (RFC 7692) compresses each message with raw DEFLATE.
// With context takeover — the default — the LZ77 window is shared across the
// messages of one direction, so a message is inflated against the last 32 KiB
// of what the earlier ones decompressed to. Each compressed message has the
// four trailing bytes 0x00 0x00 0xFF 0xFF removed by the sender; they are
// added back, followed by a final empty block, so the DEFLATE stream ends
// cleanly.
//
// Only inflation is here: the proxy offers editing only on connections whose
// compression it stripped at the handshake (see Negotiation), so an edited
// message is always written uncompressed and nothing is ever deflated.

// tail is the stripped sync-flush marker plus a final empty stored block, so
// flate reads one message to a clean EOF (the technique gorilla/websocket
// uses).
const tail = "\x00\x00\xff\xff\x01\x00\x00\xff\xff"

// dictSize is DEFLATE's sliding window.
const dictSize = 32 << 10

// inflater inflates one direction's messages, keeping the window between
// them when context takeover is in use.
type inflater struct {
	contextTakeover bool
	dict            []byte // the last dictSize bytes decompressed so far
}

func newInflater(contextTakeover bool) *inflater {
	return &inflater{contextTakeover: contextTakeover}
}

// inflate decompresses one message's payload. truncated says the payload was
// cut at the size limit, so an error at the end is expected. max bounds the
// decompressed size.
func (in *inflater) inflate(payload []byte, truncated bool, max int) ([]byte, error) {
	src := io.MultiReader(bytes.NewReader(payload), strings.NewReader(tail))
	var fr io.ReadCloser
	if in.contextTakeover {
		fr = flate.NewReaderDict(src, in.dict)
	} else {
		fr = flate.NewReader(src)
	}
	defer fr.Close()

	var out bytes.Buffer
	_, err := io.CopyN(&out, fr, int64(max)+1)
	switch {
	case err == nil || err == io.EOF:
	case truncated:
		// A truncated capture won't inflate cleanly; keep what came out.
	default:
		return nil, err
	}
	data := out.Bytes()
	if len(data) > max {
		data = data[:max]
	}
	if in.contextTakeover {
		in.updateDict(data)
	}
	return data, nil
}

func (in *inflater) updateDict(data []byte) {
	in.dict = append(in.dict, data...)
	if len(in.dict) > dictSize {
		in.dict = in.dict[len(in.dict)-dictSize:]
	}
}
