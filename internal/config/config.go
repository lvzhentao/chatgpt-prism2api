package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is process-wide runtime config. Vendor URLs have no product defaults.
type Config struct {
	ListenAddr     string
	APIBaseURL     string
	WebsiteURL     string
	ClientVersion  string
	ClientType     string
	APIKey         string
	CredentialDir  string
	MockMode       bool
	MockAddr       string
	Timeout        time.Duration
	OpenBrowser    bool
	APIKeyAuth     string
	APIKeys        []string
	DefaultModel   string
	ModelMap       map[string]string
	DatabaseURL    string
	EncryptKey     string
	TLSCertFile    string
	TLSKeyFile     string
	ProxyURL       string
	AdminStaticDir string
	DebugDir       string
}

func Default() *Config {
	c := &Config{
		ListenAddr:    "127.0.0.1:8080",
		APIBaseURL:    "https://api.example.com",
		WebsiteURL:    "https://example.com",
		ClientVersion: "1.0.0",
		ClientType:    "web",
		CredentialDir: credentialDir(),
		MockAddr:      "127.0.0.1:9100",
		Timeout:       120 * time.Second,
		OpenBrowser:   true,
		DefaultModel:  "vendor-default",
		ModelMap:      make(map[string]string),
	}
	if v := os.Getenv("VENDOR_API_BASE_URL"); v != "" {
		c.APIBaseURL = v
	}
	if v := os.Getenv("VENDOR_WEBSITE_URL"); v != "" {
		c.WebsiteURL = v
	}
	if v := os.Getenv("VENDOR_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WEB2API_CLIENT_VERSION"); v != "" {
		c.ClientVersion = v
	}
	if v := os.Getenv("WEB2API_CLIENT_TYPE"); v != "" {
		c.ClientType = v
	}
	if v := os.Getenv("WEB2API_API_KEY"); v != "" {
		c.APIKeyAuth = v
	}
	if v := os.Getenv("WEB2API_API_KEYS"); v != "" {
		c.APIKeys = splitKeys(v)
	}
	if v := os.Getenv("WEB2API_DEFAULT_MODEL"); v != "" {
		c.DefaultModel = v
	}
	if v := os.Getenv("WEB2API_MODEL_MAP"); v != "" {
		c.ModelMap = parseModelMap(v)
	}
	if v := os.Getenv("WEB2API_DATABASE_URL"); v != "" {
		c.DatabaseURL = v
	} else if v := os.Getenv("DATABASE_URL"); v != "" {
		c.DatabaseURL = v
	}
	if v := os.Getenv("WEB2API_ENCRYPT_KEY"); v != "" {
		c.EncryptKey = v
	}
	if v := os.Getenv("WEB2API_TLS_CERT"); v != "" {
		c.TLSCertFile = v
	}
	if v := os.Getenv("WEB2API_TLS_KEY"); v != "" {
		c.TLSKeyFile = v
	}
	if v := os.Getenv("WEB2API_PROXY"); v != "" {
		c.ProxyURL = v
	}
	if v := os.Getenv("WEB2API_ADMIN_STATIC"); v != "" {
		c.AdminStaticDir = v
	} else {
		c.AdminStaticDir = "web/dist"
	}
	if v := os.Getenv("WEB2API_DEBUG_DIR"); v != "" {
		c.DebugDir = v
	} else {
		switch strings.ToLower(strings.TrimSpace(os.Getenv("WEB2API_DEBUG"))) {
		case "1", "true", "yes", "on":
			c.DebugDir = "local/debug"
		}
	}
	return c
}

func parseModelMap(s string) map[string]string {
	res := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		var k, v string
		if idx := strings.IndexAny(pair, "=:"); idx != -1 {
			k = strings.TrimSpace(pair[:idx])
			v = strings.TrimSpace(pair[idx+1:])
		}
		if k != "" && v != "" {
			res[strings.ToLower(k)] = v
		}
	}
	return res
}

func splitKeys(s string) []string {
	var out []string
	for _, k := range strings.Split(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

func ParseFlags() *Config {
	c := Default()
	flag.StringVar(&c.ListenAddr, "listen", c.ListenAddr, "API listen address")
	flag.StringVar(&c.APIBaseURL, "endpoint", c.APIBaseURL, "Vendor API base URL")
	flag.StringVar(&c.APIKey, "api-key", c.APIKey, "Bootstrap vendor API key")
	flag.StringVar(&c.DefaultModel, "default-model", c.DefaultModel, "Default model id")
	flag.Func("api-keys", "Comma-separated vendor API keys (one account each)", func(v string) error {
		c.APIKeys = splitKeys(v)
		return nil
	})
	flag.Func("model-map", "Model aliases k=v,k2=v2", func(v string) error {
		for k, val := range parseModelMap(v) {
			c.ModelMap[k] = val
		}
		return nil
	})
	flag.StringVar(&c.APIKeyAuth, "auth-key", c.APIKeyAuth, "Gateway API key (Authorization: Bearer)")
	flag.BoolVar(&c.MockMode, "mock", c.MockMode, "In-process mock vendor")
	flag.StringVar(&c.MockAddr, "mock-addr", c.MockAddr, "Unused when mock is in-process")
	flag.DurationVar(&c.Timeout, "timeout", c.Timeout, "Upstream timeout")
	flag.BoolVar(&c.OpenBrowser, "browser", c.OpenBrowser, "Open browser on login")
	flag.StringVar(&c.DatabaseURL, "database-url", c.DatabaseURL, "PostgreSQL URL")
	flag.StringVar(&c.EncryptKey, "encrypt-key", c.EncryptKey, "Token encryption key")
	flag.StringVar(&c.TLSCertFile, "tls-cert", c.TLSCertFile, "TLS cert")
	flag.StringVar(&c.TLSKeyFile, "tls-key", c.TLSKeyFile, "TLS key")
	flag.StringVar(&c.ProxyURL, "proxy", c.ProxyURL, "Global upstream HTTP proxy")
	flag.StringVar(&c.AdminStaticDir, "admin-static", c.AdminStaticDir, "Admin SPA directory")
	flag.StringVar(&c.DebugDir, "debug-dir", c.DebugDir, "JSONL debug dir")
	flag.Parse()
	return c
}

func credentialDir() string {
	if v := os.Getenv("WEB2API_CREDENTIAL_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("AGENT_CLI_CREDENTIAL_STORE_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".prism-2api"
	}
	return filepath.Join(home, ".prism-2api")
}

func (c *Config) CredentialFile() string {
	return filepath.Join(c.CredentialDir, "credentials.json")
}

func (c *Config) String() string {
	return fmt.Sprintf("listen=%s endpoint=%s mock=%v", c.ListenAddr, c.APIBaseURL, c.MockMode)
}
