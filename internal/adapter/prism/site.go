package prism

import (
	"prism-2api/internal/adapter"
)

// DisplayName is the site key shown in logs /admin. scaffold/new_site.py replaces this.
const DisplayName = "prism"

// Adapter is the vendor implementation. Scaffold renames this package to the site key.
type Adapter struct {
	apiBase       string
	websiteURL    string
	clientVersion string
	clientType    string
	mock          bool
}

// Config is boot-time vendor settings (from env / flags).
type Config struct {
	APIBaseURL    string
	WebsiteURL    string
	ClientVersion string
	ClientType    string
	Mock          bool
}

// New wires a site adapter. cmd/server must adapter.Bind the result.
func New(cfg Config) *Adapter {
	ct := cfg.ClientType
	if ct == "" {
		ct = "web"
	}
	return &Adapter{
		apiBase:       cfg.APIBaseURL,
		websiteURL:    cfg.WebsiteURL,
		clientVersion: cfg.ClientVersion,
		clientType:    ct,
		mock:          cfg.Mock,
	}
}

func (a *Adapter) Name() string { return DisplayName }

func (a *Adapter) NewClient(cfg adapter.ClientConfig) adapter.Client {
	if a.mock {
		return newMockClient()
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = a.apiBase
	}
	if cfg.ClientVersion == "" {
		cfg.ClientVersion = a.clientVersion
	}
	if cfg.ClientType == "" {
		cfg.ClientType = a.clientType
	}
	return newHTTPClient(cfg)
}

func (a *Adapter) NewCatalog() adapter.Catalog {
	return adapter.NewStaticCatalog(staticModels())
}

func (a *Adapter) Auth() adapter.Auth { return a }

func (a *Adapter) MapChat(req adapter.ChatRequest) (*adapter.NativeRequest, error) {
	return mapChat(req)
}
