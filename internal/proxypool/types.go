package proxypool

import "time"

// Proxy 是一条可复用的出口配置。账号绑定 proxy_id；未绑定则走默认或运行时全局。
type Proxy struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"` // resin | http | direct
	Enabled     bool      `json:"enabled"`
	IsDefault   bool      `json:"is_default,omitempty"`
	Notes       string    `json:"notes,omitempty"`
	HTTPProxy   string    `json:"http_proxy,omitempty"`
	ResinURL    string    `json:"resin_url,omitempty"`
	ResinPlat   string    `json:"resin_platform,omitempty"`
	LastProbeIP string    `json:"last_probe_ip,omitempty"`
	LastProbeAt int64     `json:"last_probe_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CreateRequest 新建出口。
type CreateRequest struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Enabled   *bool  `json:"enabled"`
	IsDefault bool   `json:"is_default"`
	Notes     string `json:"notes"`
	HTTPProxy string `json:"http_proxy"`
	ResinURL  string `json:"resin_url"`
	ResinPlat string `json:"resin_platform"`
}

// PatchRequest 部分更新。
type PatchRequest struct {
	Name      *string `json:"name"`
	Kind      *string `json:"kind"`
	Enabled   *bool   `json:"enabled"`
	IsDefault *bool   `json:"is_default"`
	Notes     *string `json:"notes"`
	HTTPProxy *string `json:"http_proxy"`
	ResinURL  *string `json:"resin_url"`
	ResinPlat *string `json:"resin_platform"`
}
