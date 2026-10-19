package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// maxHeadSize bounds a start line plus headers. Real browsers stay far below.
const maxHeadSize = 1 << 20

var errHeadTooLarge = errors.New("header block too large")

// readHead reads one start line and its headers, up to and including the
// blank line that ends them, exactly as sent. Blank lines before the start
// line are skipped, as RFC 9112 allows. io.EOF means the peer closed cleanly
// between messages.
func readHead(br *bufio.Reader) ([]byte, error) {
	var head []byte
	lineStart := 0
	for {
		chunk, err := br.ReadSlice('\n')
		head = append(head, chunk...)
		if len(head) > maxHeadSize {
			return nil, errHeadTooLarge
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // long line; keep reading it
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(head) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		}
		line := head[lineStart:]
		if isBlankLine(line) {
			if lineStart == 0 {
				head, lineStart = head[:0], 0 // leading blank line
				continue
			}
			return head, nil
		}
		lineStart = len(head)
	}
}

func isBlankLine(l []byte) bool {
	return len(l) == 1 && l[0] == '\n' || len(l) == 2 && l[0] == '\r' && l[1] == '\n'
}

type bodyKind int

const (
	bodyNone bodyKind = iota
	bodyLength
	bodyChunked
	bodyUntilClose // response delimited by the server closing the connection
)

type framing struct {
	kind   bodyKind
	length int64
}

func parseRequestHead(head []byte) (*http.Request, framing, error) {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil {
		return nil, framing{}, err
	}
	switch {
	case isChunkedTE(req.TransferEncoding):
		return req, framing{kind: bodyChunked}, nil
	case req.ContentLength > 0:
		return req, framing{kind: bodyLength, length: req.ContentLength}, nil
	}
	return req, framing{kind: bodyNone}, nil
}

func parseResponseHead(head []byte, method string) (*http.Response, framing, error) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(head)), nil)
	if err != nil {
		return nil, framing{}, err
	}
	code := resp.StatusCode
	switch {
	case method == http.MethodHead, code/100 == 1, code == http.StatusNoContent, code == http.StatusNotModified:
		return resp, framing{kind: bodyNone}, nil
	case isChunkedTE(resp.TransferEncoding):
		return resp, framing{kind: bodyChunked}, nil
	case resp.ContentLength == 0:
		return resp, framing{kind: bodyNone}, nil
	case resp.ContentLength > 0:
		return resp, framing{kind: bodyLength, length: resp.ContentLength}, nil
	}
	return resp, framing{kind: bodyUntilClose}, nil
}

func isChunkedTE(te []string) bool {
	return len(te) > 0 && strings.EqualFold(te[len(te)-1], "chunked")
}

// copyBody streams a body from src to dst exactly as framed on the wire
// (chunk-size lines and trailers included), mirroring it into rec. Bytes
// reach dst as soon as they arrive, so streamed responses (server-sent
// events, long downloads) aren't delayed.
func copyBody(dst io.Writer, src *bufio.Reader, f framing, rec io.Writer) error {
	w := io.MultiWriter(dst, rec)
	switch f.kind {
	case bodyLength:
		_, err := io.CopyN(w, src, f.length)
		return err
	case bodyUntilClose:
		_, err := io.Copy(w, src)
		return err
	case bodyChunked:
		return copyChunked(w, src)
	}
	return nil
}

func copyChunked(w io.Writer, src *bufio.Reader) error {
	for {
		line, err := readLine(src)
		if err != nil {
			return err
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		sizeField, _, _ := strings.Cut(strings.TrimSpace(string(line)), ";")
		size, err := strconv.ParseInt(strings.TrimSpace(sizeField), 16, 64)
		if err != nil || size < 0 {
			return fmt.Errorf("bad chunk size %q", sizeField)
		}
		if size == 0 {
			// Trailers, then the terminating blank line.
			for {
				line, err := readLine(src)
				if err != nil {
					return err
				}
				if _, err := w.Write(line); err != nil {
					return err
				}
				if isBlankLine(line) {
					return nil
				}
			}
		}
		if _, err := io.CopyN(w, src, size); err != nil {
			return err
		}
		crlf, err := readLine(src)
		if err != nil {
			return err
		}
		if _, err := w.Write(crlf); err != nil {
			return err
		}
	}
}

// readLine reads one line including its terminator, bounded in length.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxHeadSize {
			return nil, errHeadTooLarge
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		return line, nil
	}
}

// capped records up to limit bytes and counts the rest.
type capped struct {
	buf   []byte
	limit int
	size  int64
}

func (c *capped) Write(p []byte) (int, error) {
	c.size += int64(len(p))
	if room := c.limit - len(c.buf); room > 0 {
		c.buf = append(c.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (c *capped) truncated() bool { return c.size > int64(len(c.buf)) }
