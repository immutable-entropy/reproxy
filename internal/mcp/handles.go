package mcp

import (
	"fmt"
	"strconv"
	"strings"
)

// Handles name a flow, a Replay session or a finding across every instance
// in the group, so a tool call can point at one unambiguously. A flow is
// "<instanceID>.f<flowID>", a Replay session is "<instanceID>.r<sessionID>"
// and a finding is "<instanceID>.n<findingID>". The instance ID is hex (see
// hub.NewInstanceID), so the separator is the last dot.

const (
	flowMark    = 'f'
	sessionMark = 'r'
	findingMark = 'n'
)

// flowHandle formats a flow handle.
func flowHandle(instanceID string, flowID uint64) string {
	return fmt.Sprintf("%s.%c%d", instanceID, flowMark, flowID)
}

// sessionHandle formats a Replay session handle.
func sessionHandle(instanceID string, sessionID uint64) string {
	return fmt.Sprintf("%s.%c%d", instanceID, sessionMark, sessionID)
}

// findingHandle formats a finding handle.
func findingHandle(instanceID string, findingID uint64) string {
	return fmt.Sprintf("%s.%c%d", instanceID, findingMark, findingID)
}

// parseFlow splits a flow handle into its instance and flow ID.
func parseFlow(h string) (instanceID string, flowID uint64, err error) {
	return parseHandle(h, flowMark, "flow")
}

// parseSession splits a session handle into its instance and session ID.
func parseSession(h string) (instanceID string, sessionID uint64, err error) {
	return parseHandle(h, sessionMark, "replay session")
}

// parseFinding splits a finding handle into its instance and finding ID.
func parseFinding(h string) (instanceID string, findingID uint64, err error) {
	return parseHandle(h, findingMark, "finding")
}

func parseHandle(h string, mark byte, kind string) (string, uint64, error) {
	// The instance ID is hex, so the handle's own separator is the last dot.
	i := strings.LastIndexByte(h, '.')
	malformed := fmt.Errorf("%q is not a %s handle (want <instance>.%c<id>)", h, kind, mark)
	if i <= 0 {
		return "", 0, malformed
	}
	inst, rest := h[:i], h[i+1:]
	if len(rest) < 2 || rest[0] != mark {
		return "", 0, malformed
	}
	id, err := strconv.ParseUint(rest[1:], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("%q is not a %s handle: %w", h, kind, err)
	}
	return inst, id, nil
}
