package adapter

import (
	"encoding/json"
	"net/http"

	"prism-2api/internal/egress"
)

// ClientConfig is how the kernel constructs a per-account client.
type ClientConfig struct {
	BaseURL       string
	ClientVersion string
	ClientType    string
	TokenProvider func() (string, error)
}

// ChatRequest is the kernel's vendor-neutral chat request (OpenAI-shaped).
type ChatRequest struct {
	Model           string
	Messages        []ChatMessage
	Tools           []ToolDef
	Stream          bool
	Temperature     *float64
	MaxTokens       int
	ReasoningEffort string
	SystemPrompt    string // 已解析好的注入指令；空=不注入（api 层每请求从 runtime 热读填入）
}

// ChatMessage is one turn. Content is already flattened to text; attachments are separate.
type ChatMessage struct {
	Role       string
	Content    string
	Files      []File
	ToolCalls  []ToolCall
	ToolCallID string
	Reasoning  string
}

// File is one binary attachment carried with a message (image, PDF, plain text…).
// Data is the decoded payload; Name/Mime describe it to the vendor.
// Text is the pre-extracted text content for text-like documents (empty for images):
// the api layer fills it so vendors without binary input can inline it.
type File struct {
	Name string
	Mime string
	Data []byte
	Text string
}

// ToolDef is an OpenAI function tool.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall is a completed tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// NativeRequest is what Site.MapChat produces. Kernel never inspects Extra.
type NativeRequest struct {
	Model        string
	Messages     []ChatMessage
	Tools        []ToolDef
	Stream       bool
	Temperature  *float64
	MaxTokens    int
	Thinking     string
	MaxMode      bool
	SystemPrompt string // 注入指令最终文本；空=由站点自行决定（prism 用 env/默认路径）
	Extra        map[string]any
}

// Event is one upstream stream frame, already mapped off the vendor wire.
type Event struct {
	Text             string
	IntermediateText string
	Thinking         *Thinking
	ToolCall         *StreamedToolCall
	PartialToolCall  *PartialToolCall
	StatusUpdate     json.RawMessage
	UsageUUID        string
	IsSlowRequest    bool
	ContextWindow    *ContextWindowStatus
	InputTokens      int32
	OutputTokens     int32
	Ended            bool
	Err              error
}

// Thinking is a reasoning delta.
type Thinking struct {
	Text                string
	Signature           string
	RedactedThinking    string
	IsLastThinkingChunk bool
}

// StreamedToolCall is a completed tool call from the model.
type StreamedToolCall struct {
	Tool        string
	ToolCallID  string
	Name        string
	RawArgs     string
	ToolIndex   uint32
	ModelCallID string
	Kind        string
}

// PartialToolCall is a streaming tool-call start (name known, args still arriving).
type PartialToolCall struct {
	ToolCallID string
	Name       string
}

// ContextWindowStatus is optional upstream context occupancy.
type ContextWindowStatus struct {
	TokensUsed     int64   `json:"tokensUsed,omitempty"`
	TokenLimit     int64   `json:"tokenLimit,omitempty"`
	PercentageUsed float64 `json:"percentageUsed,omitempty"`
}

// ToolCallInfo is the kernel tool-mapper's input (replaces vendor protobuf).
type ToolCallInfo struct {
	ExecID     string
	Kind       string
	ToolName   string
	ToolCallID string
	ArgsJSON   string
}

// ModelInfo is one catalog entry.
type ModelInfo struct {
	ID                string
	ServerModelName   string
	DisplayName       string
	Aliases           []string
	SupportsThinking  bool
	SupportsImages    bool
	SupportsAgent     bool
	SupportsMaxMode   bool
	ContextTokenLimit int32
	Price             float64
	MaxMode           bool
	ThinkingLevel     string
}

// UsageSnapshot is official-quota cache shown on the admin usage page.
// Field names stay stable so the SPA meters keep working; fill what the vendor has.
type UsageSnapshot struct {
	Email              string   `json:"email,omitempty"`
	WorkOSID           string   `json:"workos_id,omitempty"`
	SignUpType         string   `json:"sign_up_type,omitempty"`
	MembershipType     string   `json:"membership_type,omitempty"`
	IndividualPlan     string   `json:"individual_membership_type,omitempty"`
	TeamMembershipType string   `json:"team_membership_type,omitempty"`
	SubscriptionStatus string   `json:"subscription_status,omitempty"`
	IsTeamMember       *bool    `json:"is_team_member,omitempty"`
	IsEnterprise       *bool    `json:"is_enterprise,omitempty"`
	PlanBadge          string   `json:"plan_badge,omitempty"`
	PlanLabel          string   `json:"plan_label,omitempty"`
	BillingCycleStart  int64    `json:"billing_cycle_start,omitempty"`
	BillingCycleEnd    int64    `json:"billing_cycle_end,omitempty"`
	PlanUsedCents      *float64 `json:"plan_used_cents,omitempty"`
	PlanLimitCents     *float64 `json:"plan_limit_cents,omitempty"`
	TotalPercentUsed   *float64 `json:"total_percent_used,omitempty"`
	AutoPercentUsed    *float64 `json:"auto_percent_used,omitempty"`
	APIPercentUsed     *float64 `json:"api_percent_used,omitempty"`
	OnDemandUsedCents  *float64 `json:"on_demand_used_cents,omitempty"`
	OnDemandLimitCents *float64 `json:"on_demand_limit_cents,omitempty"`
	TeamOnDemandUsed   *float64 `json:"team_on_demand_used_cents,omitempty"`
	TeamOnDemandLimit  *float64 `json:"team_on_demand_limit_cents,omitempty"`
	OnDemandEnabled    *bool    `json:"on_demand_enabled,omitempty"`
	OnDemandLimitType  string   `json:"on_demand_limit_type,omitempty"`
	Unlimited          bool     `json:"unlimited,omitempty"`
	FetchedAt          int64    `json:"fetched_at,omitempty"`
}

const quotaFullPercent = 99.5

// HasQuotaMeters reports whether any billable bucket is present.
func (s UsageSnapshot) HasQuotaMeters() bool {
	return s.APIPercentUsed != nil || s.AutoPercentUsed != nil || s.TotalPercentUsed != nil ||
		(s.PlanUsedCents != nil && s.PlanLimitCents != nil)
}

// QuotaDisableReason returns a disable reason when a bucket is full.
func (s UsageSnapshot) QuotaDisableReason() string {
	if s.Unlimited {
		return ""
	}
	full := func(p *float64) bool { return p != nil && *p >= quotaFullPercent }
	if full(s.APIPercentUsed) {
		return "quota_api"
	}
	if full(s.AutoPercentUsed) {
		return "quota_auto"
	}
	if full(s.TotalPercentUsed) {
		return "quota_total"
	}
	if s.PlanUsedCents != nil && s.PlanLimitCents != nil && *s.PlanLimitCents > 0 &&
		(*s.PlanUsedCents / *s.PlanLimitCents)*100 >= quotaFullPercent {
		return "quota_plan"
	}
	return ""
}

// JoinURL joins base + path without double slashes.
func JoinURL(base, path string) string {
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	if path == "" {
		return base
	}
	if path[0] != '/' {
		path = "/" + path
	}
	return base + path
}

// ApplyEgress is a helper sites can call after constructing an HTTP client.
func ApplyEgress(c *http.Client, s egress.Settings) error {
	_ = c
	_ = s
	return nil
}
