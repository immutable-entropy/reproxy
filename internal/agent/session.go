package agent

import "time"

// Mode is how much an agent session may do without asking the user.
type Mode string

const (
	// ModeAsk prompts before anything that sends or changes traffic.
	ModeAsk Mode = "ask"
	// ModeAuto lets the agent act within the scope rule without asking.
	ModeAuto Mode = "auto"
)

// Role is who produced a message in a transcript.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool records a tool call and its result, shown as a card.
	RoleTool Role = "tool"
	// RoleSystem records notices (errors, "agent stopped"), shown muted.
	RoleSystem Role = "system"
)

// Status is a session's current activity.
type Status string

const (
	// StatusIdle: waiting for the user.
	StatusIdle Status = "idle"
	// StatusRunning: a turn is in progress.
	StatusRunning Status = "running"
	// StatusError: the last turn failed; the message is in the transcript.
	StatusError Status = "error"
)

// ToolCall records one tool invocation for display.
type ToolCall struct {
	Name    string `json:"name"`
	Input   string `json:"input,omitempty"`  // compact JSON
	Output  string `json:"output,omitempty"` // truncated for display
	IsError bool   `json:"is_error,omitempty"`
	// ReplaySession, when set, is the "<instance>.r<id>" handle of a Replay
	// session the call created, so the UI can offer "Open in Replay".
	ReplaySession string `json:"replay_session,omitempty"`
}

// Message is one entry in a transcript.
type Message struct {
	Role Role      `json:"role"`
	Text string    `json:"text"`
	Tool *ToolCall `json:"tool,omitempty"`
	At   time.Time `json:"at"`
}

// Session is one agent chat. Transcripts are persisted so sessions survive
// a hub handover.
type Session struct {
	ID        string    `json:"id"`
	ProfileID string    `json:"profile_id"`
	Title     string    `json:"title"`
	Mode      Mode      `json:"mode"`
	Status    Status    `json:"status"`
	Created   time.Time `json:"created"`
	Messages  []Message `json:"messages"`
	// Managed marks a session a background service owns and controls, rather
	// than the user: the findings reviewer's session (see package app). The UI
	// shows it but refuses to close it — it is turned off through its service's
	// configuration — so it can't be left running unseen or shut without the
	// service knowing.
	Managed bool `json:"managed,omitempty"`
	// Convo is the provider-neutral conversation the Runner drives the model
	// with. It is persisted (so a session survives a restart with its full
	// context, including any provider raw content) but never shown; Messages
	// is the display transcript.
	Convo []Entry `json:"convo,omitempty"`
}

// Summary is a session without its transcript, for the Sessions list.
type Summary struct {
	ID        string `json:"id"`
	ProfileID string `json:"profile_id"`
	Title     string `json:"title"`
	Mode      Mode   `json:"mode"`
	Status    Status `json:"status"`
	Managed   bool   `json:"managed,omitempty"`
}

func (s *Session) summary() Summary {
	return Summary{ID: s.ID, ProfileID: s.ProfileID, Title: s.Title, Mode: s.Mode, Status: s.Status, Managed: s.Managed}
}
