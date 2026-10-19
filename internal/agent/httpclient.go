package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"reproxy/internal/elide"
)

// errUnknownVendor is returned for a profile naming a vendor we don't build.
func errUnknownVendor(vendor string) error {
	return fmt.Errorf("agent: unknown vendor %q", vendor)
}

// defaultHTTP is the client providers use. Agent turns can be slow (a model
// thinking, then a tool round-trip), so the timeout is generous; the Runner
// also honours ctx cancellation (Stop) for a quicker exit.
func defaultHTTP() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

// postJSON sends v as JSON to url with the given headers and decodes the
// response into out. A non-2xx status is returned as an error carrying the
// response body, so the model (and the user) can see what the vendor said.
func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, v, out any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, snippet(data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// snippet trims an error body for a message.
func snippet(b []byte) string {
	return elide.Runes(string(bytes.TrimSpace(b)), 500)
}

// errorf returns a vendor error message as an error, prefixed so logs make
// clear it came from the model's API.
func errorf(msg string) error {
	return fmt.Errorf("provider error: %s", msg)
}
