package adapter

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"prism-2api/internal/auth"
	"prism-2api/internal/egress"
)

// Site is the only package a new chat 2API must implement.
// Kernel (pool / admin / OpenAI+Anthropic gateway / emulation) talks to this interface.
type Site interface {
	Name() string
	NewClient(cfg ClientConfig) Client
	NewCatalog() Catalog
	Auth() Auth
	MapChat(req ChatRequest) (*NativeRequest, error)
}

// Client is one account's upstream HTTP session.
type Client interface {
	Stream(ctx context.Context, req *NativeRequest, emit func(Event) bool) error
	ListModels(ctx context.Context) ([]ModelInfo, error)
	FetchUsage(ctx context.Context, websiteURL string) (UsageSnapshot, error)
	SetBaseURL(url string)
	SetEgress(s egress.Settings) error
	SetHTTPClient(c *http.Client)
}

// Catalog is the model directory + ID resolve (TTL cache lives in the kernel helper).
type Catalog interface {
	Load() ([]*ModelInfo, error)
	Refresh() ([]*ModelInfo, error)
	Resolve(id string) (serverName, thinkingLevel string, maxMode bool)
	ResolveWithThinking(id string, thinkingEnabled bool, effort string) (string, string, bool)
	PublicModelIDs() []string
	BareModelIDs() []string
	ModelIDs() []string
	SetClientProvider(fn func() Client)
	SetCacheTTL(d time.Duration)
	Invalidate()
}

// Auth is vendor login / refresh. Token storage stays in auth.TokenManager.
type Auth interface {
	LoginURL(websiteURL, challenge, uuid string) string
	PollLogin(apiBaseURL, uuid, verifier, clientVersion, clientType string, client *http.Client) (*auth.Token, error)
	ExchangeCredential(apiBaseURL, secret, clientVersion, clientType string, client *http.Client) (*auth.Token, error)
}

// Default is the process-wide site adapter. cmd/server must Bind it at boot.
var Default Site

// Bind sets the process-wide site adapter.
func Bind(s Site) { Default = s }

func Name() string {
	if Default == nil {
		return "vendor"
	}
	return Default.Name()
}

func NewClient(cfg ClientConfig) Client {
	if Default == nil {
		return nil
	}
	return Default.NewClient(cfg)
}

func NewCatalog() Catalog {
	if Default == nil {
		return newMemoryCatalog(nil)
	}
	return Default.NewCatalog()
}

func MapChat(req ChatRequest) (*NativeRequest, error) {
	if Default == nil {
		return nil, fmt.Errorf("adapter not bound")
	}
	return Default.MapChat(req)
}
