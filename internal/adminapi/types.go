// Package adminapi 提供管理端 JSON API 类型与处理器（无 HTML / SPA）。
package adminapi

import (
	"encoding/json"
	"strings"

	"prism-2api/internal/adapter"
	"prism-2api/internal/pool"
)

// ClientKeyStore 由 W2.2 clientkeys 经 api facade 接入；未接线时 handlers 返回 503。
type ClientKeyStore interface {
	List() []ClientKey
	Create(req CreateClientKeyRequest) (*CreateClientKeyResponse, error)
	Get(id string) (*ClientKey, error)
	Update(id string, req UpdateClientKeyRequest) (*ClientKey, error)
	Delete(id string) error
}

// ClientKey 脱敏后的入口 Key（对齐 kiro ClientKeyItem，snake_case）。
type ClientKey struct {
	ID          string `json:"id"`
	MaskedKey   string `json:"masked_key"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Disabled    bool   `json:"disabled"`
	Group       string `json:"group,omitempty"`
	TotalCalls  int64  `json:"total_calls"`
	TotalTokens int64  `json:"total_tokens"`
	CreatedAt   string `json:"created_at,omitempty"`
	LastUsedAt  string `json:"last_used_at,omitempty"`
	IsSystem    bool   `json:"is_system,omitempty"`
}

// CreateClientKeyRequest 创建入口 Key。
type CreateClientKeyRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Group       string `json:"group,omitempty"`
}

// CreateClientKeyResponse 明文 key 仅创建时返回一次。
type CreateClientKeyResponse struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at,omitempty"`
}

// UpdateClientKeyRequest 更新入口 Key 元数据。
type UpdateClientKeyRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Group       *string `json:"group"`
	Disabled    *bool   `json:"disabled"`
}

// Group 分组实体。
type Group struct {
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	RPM             int    `json:"rpm,omitempty"`
	DailyMax        int    `json:"daily_max,omitempty"`
	CredentialCount int    `json:"credential_count"`
	ClientKeyCount  int    `json:"client_key_count"`
}

// CreateGroupRequest 创建分组。
type CreateGroupRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// UpdateGroupRequest 改名 / 改备注。
type UpdateGroupRequest struct {
	NewName     *string `json:"new_name"`
	Description *string `json:"description"`
	RPM         *int    `json:"rpm"`
	DailyMax    *int    `json:"daily_max"`
}

// AccountPatch 对齐任务体：PATCH /api/admin/accounts。
type AccountPatch struct {
	Name     string    `json:"name"`
	Priority *int      `json:"priority"`
	Weight   *int64    `json:"weight"`
	Enabled  *bool     `json:"enabled"`
	ProxyURL *string   `json:"proxy_url"`
	ProxyID  *string   `json:"proxy_id"`
	RPM      *int      `json:"rpm"`
	DailyMax *int      `json:"daily_max"`
	Groups   *[]string `json:"groups"`
}

// AccountImport 批量导入一条。新建需要 api_key 或 access_token / session_token。
type AccountImport struct {
	Name         string    `json:"name"`
	Email        string    `json:"email,omitempty"`
	APIKey       string    `json:"api_key,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	SessionToken string    `json:"session_token,omitempty"`
	Priority     *int      `json:"priority"`
	Weight       *int64    `json:"weight"`
	Enabled      *bool     `json:"enabled"`
	ProxyURL     *string   `json:"proxy_url"`
	ProxyID      *string   `json:"proxy_id"`
	RPM          *int      `json:"rpm"`
	DailyMax     *int      `json:"daily_max"`
	Groups       *[]string `json:"groups"`
}

func (a *AccountImport) UnmarshalJSON(data []byte) error {
	type alias AccountImport
	var aux alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*a = AccountImport(aux)
	var extra map[string]any
	if json.Unmarshal(data, &extra) != nil {
		return nil
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := extra[k]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	if a.Name == "" {
		a.Name = pick("Name")
	}
	if a.Email == "" {
		a.Email = pick("Email")
	}
	if a.APIKey == "" {
		a.APIKey = pick("apiKey", "APIKey")
	}
	if a.AccessToken == "" {
		a.AccessToken = pick("accessToken", "token", "Token")
	}
	if a.RefreshToken == "" {
		a.RefreshToken = pick("refreshToken", "RefreshToken")
	}
	if a.SessionToken == "" {
		a.SessionToken = pick("WorkosCursorSessionToken", "sessionToken", "session", "SessionToken")
	}
	return nil
}

func (p AccountImport) toPool() pool.AdminPatch {
	return pool.AdminPatch{
		Priority: p.Priority,
		Weight:   p.Weight,
		Enabled:  p.Enabled,
		ProxyURL: p.ProxyURL,
		ProxyID:  p.ProxyID,
		RPM:      p.RPM,
		DailyMax: p.DailyMax,
		Groups:   p.Groups,
	}
}

func (p AccountPatch) toPool() pool.AdminPatch {
	return pool.AdminPatch{
		Priority: p.Priority,
		Weight:   p.Weight,
		Enabled:  p.Enabled,
		ProxyURL: p.ProxyURL,
		ProxyID:  p.ProxyID,
		RPM:      p.RPM,
		DailyMax: p.DailyMax,
		Groups:   p.Groups,
	}
}

// Dashboard 管理总览：账号计数 + 冷却原因 + 请求统计 + 运行时长。
type Dashboard struct {
	UptimeSeconds   int            `json:"uptime_seconds"`
	Accounts        AccountCounts  `json:"accounts"`
	Cooldowns       []CooldownItem `json:"cooldowns"`
	RefreshFailures map[string]int `json:"refresh_failures,omitempty"`
	Stats           DashboardStats `json:"stats"`
}

// AccountCounts 池内计数。
type AccountCounts struct {
	Total    int `json:"total"`
	Enabled  int `json:"enabled"`
	Disabled int `json:"disabled"`
	Cooling  int `json:"cooling"`
	Ready    int `json:"ready"`
}

// CooldownItem 单号冷却（给以后 SPA / curl 读原因）。
type CooldownItem struct {
	Name      string `json:"name"`
	Reason    string `json:"reason"`
	FailClass string `json:"fail_class,omitempty"`
	Until     int64  `json:"until"`
}

// DashboardStats 复用 LogStore.ComputeStats 字段。
type DashboardStats struct {
	TotalRequests   int            `json:"total_requests"`
	ErrorRequests   int            `json:"error_requests"`
	ErrorRate       float64        `json:"error_rate"`
	TotalPrompt     int            `json:"total_prompt"`
	TotalCompletion int            `json:"total_completion"`
	TotalTokens     int            `json:"total_tokens"`
	AvgLatencyMs    int64          `json:"avg_latency_ms"`
	ByAccount       map[string]int `json:"by_account"`
	ByModel         map[string]int `json:"by_model"`
	ByPath          map[string]int `json:"by_path"`
	ByFailClass     map[string]int `json:"by_fail_class"`
	ByClientKey     map[string]int `json:"by_client_key"`
	TokensByAccount map[string]int `json:"tokens_by_account"`
	TokensByModel   map[string]int `json:"tokens_by_model"`
}

// UsageResponse GET /api/admin/usage。
type UsageResponse struct {
	TotalSuccess int64          `json:"total_success"`
	Accounts     []AccountUsage `json:"accounts"`
	Keys         *KeyUsage      `json:"keys,omitempty"`
}

// AccountUsage 单号成功次数 + 官方配额（cockpit usage-summary）。
type AccountUsage struct {
	Name         string                `json:"name"`
	SuccessCount int64                 `json:"success_count"`
	Vendor       adapter.UsageSnapshot `json:"vendor,omitempty"`
	UsageError   string                `json:"usage_error,omitempty"`
	UsageAt      int64                 `json:"usage_updated_at,omitempty"`
}

// KeyUsage 入口 Key 汇总（store 未就绪则省略）。
type KeyUsage struct {
	Count       int   `json:"count"`
	TotalCalls  int64 `json:"total_calls"`
	TotalTokens int64 `json:"total_tokens"`
}

// AccountExport 账号元数据导出（不含完整令牌）。
type AccountExport struct {
	Name            string   `json:"name"`
	ID              string   `json:"id,omitempty"`
	Enabled         bool     `json:"enabled"`
	Priority        int      `json:"priority"`
	Weight          int64    `json:"weight"`
	Groups          []string `json:"groups,omitempty"`
	ProxyURL        string   `json:"proxy_url,omitempty"`
	RPM             int      `json:"rpm,omitempty"`
	DailyMax        int      `json:"daily_max,omitempty"`
	HasRefreshToken bool     `json:"has_refresh_token"`
	HasAPIKey       bool     `json:"has_api_key"`
	ExpiresAt       int64    `json:"expires_at,omitempty"`
	SuccessCount    int64    `json:"success_count"`
	CooldownReason  string   `json:"cooldown_reason,omitempty"`
	CooldownUntil   int64    `json:"cooldown_until,omitempty"`
}
