// Package agent holds the AI agent side of reproxy: the vendors an agent
// can use and (later) the session runtime that talks to them.
package agent

// API styles a vendor speaks.
const (
	APIAnthropic = "anthropic"     // Anthropic Messages API
	APIGemini    = "gemini"        // Google Gemini generateContent API
	APIOpenAI    = "openai_compat" // OpenAI-compatible chat completions
)

// Vendor describes a provider the user can add in Settings → Agents.
type Vendor struct {
	ID   string // stored in config.AgentProfile.Vendor
	Name string // shown to the user
	API  string // which wire protocol it speaks
	// DefaultBaseURL is the API endpoint used when a profile leaves BaseURL
	// empty.
	DefaultBaseURL string
	// DefaultModel pre-fills a new profile; the user can change it.
	DefaultModel string
	// NeedsKey is false for local runtimes (Ollama) that take no API key.
	NeedsKey bool
}

// Vendors lists every supported vendor, in the order offered to the user.
//
// Default models are a starting point only: vendors rename models often,
// so the model field stays editable.
var Vendors = []Vendor{
	{ID: "anthropic", Name: "Anthropic (Claude)", API: APIAnthropic,
		DefaultBaseURL: "https://api.anthropic.com", DefaultModel: "claude-opus-5-5", NeedsKey: true},
	{ID: "gemini", Name: "Google Gemini", API: APIGemini,
		DefaultBaseURL: "https://generativelanguage.googleapis.com", DefaultModel: "gemini-2.5-pro", NeedsKey: true},
	{ID: "ollama", Name: "Ollama (local)", API: APIOpenAI,
		DefaultBaseURL: "http://localhost:11434/v1", DefaultModel: "qwen3", NeedsKey: false},
	{ID: "deepseek", Name: "DeepSeek", API: APIOpenAI,
		DefaultBaseURL: "https://api.deepseek.com", DefaultModel: "deepseek-chat", NeedsKey: true},
	{ID: "qwen", Name: "Qwen (Alibaba DashScope)", API: APIOpenAI,
		DefaultBaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", DefaultModel: "qwen-plus", NeedsKey: true},
	{ID: "kimi", Name: "Kimi (Moonshot)", API: APIOpenAI,
		DefaultBaseURL: "https://api.moonshot.ai/v1", DefaultModel: "kimi-k2-turbo-preview", NeedsKey: true},
}

// VendorByID returns the vendor with the given ID.
func VendorByID(id string) (Vendor, bool) {
	for _, v := range Vendors {
		if v.ID == id {
			return v, true
		}
	}
	return Vendor{}, false
}
