package prism

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 侧车登录：auth.openai.com 对非浏览器客户端一律回 Cloudflare JS 挑战
//（实测：普通 HTTP、TLS 指纹伪装、复用浏览器 cf_clearance 都是 403），
// 所以登录交给真浏览器侧车（Camoufox；本地开发可用 chromium），
// Go 侧只负责喂参数、收事件、落库。

// LoginRequest 是一次侧车登录任务的输入。
type LoginRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	TOTPSecret string `json:"totp_secret,omitempty"`
	Proxy      string `json:"proxy,omitempty"`
	// Backend: camoufox（生产，反检测 Firefox）| chromium（本地开发兜底）。
	Backend string `json:"backend,omitempty"`
	// Headless=false 时开有头浏览器：实测 headless 过不了 Cloudflare 挑战。
	Headless bool `json:"headless"`
	Timeout  int  `json:"timeout,omitempty"`
	// StorageStateIn/Out 复用设备指纹，降低风控概率。
	StorageStateIn  string `json:"storage_state_in,omitempty"`
	StorageStateOut string `json:"storage_state_out,omitempty"`
}

// LoginResult 是登录成功的产出。
type LoginResult struct {
	// OAI 是 prism_oai_access_token（入库主凭据，约 10 天）。
	OAI string `json:"-"`
	// Refresh 是 prism_oai_refresh_token（长期，用于续期）。
	Refresh string `json:"-"`
	// Cookies 是全量 cookie（含 session / device）。
	Cookies       map[string]string `json:"cookies"`
	StorageState  string            `json:"storage_state,omitempty"`
	URL           string            `json:"url,omitempty"`
	LastChallenge bool              `json:"-"`
}

// LoginProgress 是侧车进度事件（可直接写进任务日志）。
type LoginProgress func(state, detail string)

type sidecarEvent struct {
	Event   string            `json:"event"`
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Message string            `json:"message"`
	Stage   string            `json:"stage"`
	Cookies map[string]string `json:"cookies"`
	State   string            `json:"storage_state"`
	Pong    bool              `json:"pong"`
}

// SidecarPath 返回侧车脚本路径（可用 PRISM_LOGIN_SIDECAR 覆盖）。
func SidecarPath() string {
	if p := strings.TrimSpace(os.Getenv("PRISM_LOGIN_SIDECAR")); p != "" {
		return p
	}
	candidates := []string{
		filepath.Join("sidecar", "login", "login.py"),
		filepath.Join("..", "sidecar", "login", "login.py"),
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "sidecar", "login", "login.py"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}

func sidecarPython() string {
	if p := strings.TrimSpace(os.Getenv("PRISM_LOGIN_PYTHON")); p != "" {
		return p
	}
	return "python3"
}

// SidecarURL 返回远端侧车地址（线上：网关与侧车是两个容器，用 HTTP 驱动）。
// 设了 PRISM_LOGIN_SIDECAR_URL 就走 HTTP，不再 exec 本地脚本。
func SidecarURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("PRISM_LOGIN_SIDECAR_URL")), "/")
}

// RunSidecarLogin 跑一次登录：配了 PRISM_LOGIN_SIDECAR_URL 走远端 HTTP 侧车，
// 否则 exec 本地脚本（本地开发）。两条路的请求/事件协议完全一致。
func RunSidecarLogin(ctx context.Context, req LoginRequest, progress LoginProgress) (*LoginResult, error) {
	backend := strings.TrimSpace(req.Backend)
	if backend == "" {
		backend = "camoufox"
	}
	if url := SidecarURL(); url != "" {
		return runRemoteLogin(ctx, url, req, progress)
	}
	return runLocalLogin(ctx, req, backend, progress)
}

// runRemoteLogin 走 HTTP 侧车（NDJSON 流式事件）。
func runRemoteLogin(ctx context.Context, baseURL string, req LoginRequest, progress LoginProgress) (*LoginResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/login", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/x-ndjson")
	resp, err := loginHTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("prism: login sidecar %s: %w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("prism: login sidecar %s: status %d: %s", baseURL, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return consumeSidecarEvents(resp.Body, progress)
}

// runLocalLogin 起本地侧车进程跑一次登录。
func runLocalLogin(ctx context.Context, req LoginRequest, backend string, progress LoginProgress) (*LoginResult, error) {
	script := SidecarPath()
	if _, err := os.Stat(script); err != nil {
		return nil, fmt.Errorf("prism: login sidecar not found at %s (set PRISM_LOGIN_SIDECAR or PRISM_LOGIN_SIDECAR_URL)", script)
	}

	cmd := exec.CommandContext(ctx, sidecarPython(), script, "--backend", backend)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("prism: start login sidecar: %w", err)
	}

	// 侧车的 stderr 是调试日志，转到本进程 stderr 方便排障。
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(os.Stderr, stderr)
	}()

	encoded, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := stdin.Write(append(encoded, '\n')); err != nil {
		return nil, fmt.Errorf("prism: write sidecar request: %w", err)
	}
	_ = stdin.Close()

	result, sidecarErr := consumeSidecarEvents(stdout, progress)
	wg.Wait()
	_ = cmd.Wait()
	if sidecarErr != nil {
		return nil, sidecarErr
	}
	return result, nil
}

// loginHTTPClient 是远端侧车的 HTTP 客户端：登录本身可能跑几分钟，不设总超时（靠 ctx）。
func loginHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 nil,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}}
}

// consumeSidecarEvents 读事件流（本地 stdout / 远端 NDJSON），返回登录结果。
func consumeSidecarEvents(r io.Reader, progress LoginProgress) (*LoginResult, error) {
	result := &LoginResult{}
	var sidecarErr error
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev sidecarEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Event {
		case "state":
			if progress != nil {
				detail := ev.URL
				if detail == "" {
					detail = ev.Name
				}
				progress(ev.Name, detail)
			}
		case "done":
			if ev.Cookies != nil {
				result.Cookies = ev.Cookies
				result.OAI = ev.Cookies["prism_oai_access_token"]
				result.Refresh = ev.Cookies["prism_oai_refresh_token"]
			}
			result.StorageState = ev.State
			result.URL = ev.URL
		case "error":
			sidecarErr = fmt.Errorf("login sidecar: %s: %s", ev.Stage, ev.Message)
		}
	}
	if sidecarErr != nil {
		return nil, sidecarErr
	}
	if result.OAI == "" {
		return nil, fmt.Errorf("prism: login sidecar finished without prism_oai_access_token")
	}
	return result, nil
}

// ---------------------------------------------------------------- 凭据束

// CredentialBundle 是存进 auth.Token.RefreshToken 的重登材料：
// 内核 keepalive 到点会把它当作 exchange secret 交回 ExchangeCredential，
// 于是「token 将过期」自动变成「重新登录」。
type CredentialBundle struct {
	OAI       string `json:"prism_oai_access_token,omitempty"`
	Refresh   string `json:"prism_oai_refresh_token,omitempty"`
	Email     string `json:"email,omitempty"`
	Password  string `json:"password,omitempty"`
	TOTP      string `json:"totp_secret,omitempty"`
	Proxy     string `json:"proxy,omitempty"`
	Backend   string `json:"backend,omitempty"`
	Headless  bool   `json:"headless,omitempty"`
	StatePath string `json:"storage_state,omitempty"`
}

// BundleForImport 组一份「待登录」凭据束（不登录、不入库，只把登录材料交给内核存着）：
// 导入账号密码 + TOTP 时用它填 AccountImport.RefreshToken —— 登录成功后内核
// keepalive 到点就能自动重登。
func BundleForImport(email, password, totp, proxy string) string {
	b := &CredentialBundle{
		Email:    strings.TrimSpace(email),
		Password: password,
		TOTP:     strings.TrimSpace(totp),
		Proxy:    strings.TrimSpace(proxy),
	}
	if !b.CanRelogin() {
		return ""
	}
	return b.Marshal()
}

// ParseCredentialBundle 尝试按凭据束解析；不像就返回 nil。
func ParseCredentialBundle(raw string) *CredentialBundle {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		return nil
	}
	var b CredentialBundle
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return nil
	}
	if b.Email == "" && b.OAI == "" && b.Refresh == "" {
		return nil
	}
	return &b
}

// CanRelogin 判断这份凭据束是否具备重登能力。
func (b *CredentialBundle) CanRelogin() bool {
	return b != nil && b.Email != "" && b.Password != ""
}

// Marshal 序列化凭据束（存进 RefreshToken）。
func (b *CredentialBundle) Marshal() string {
	raw, _ := json.Marshal(b)
	return string(raw)
}

// ReloginWithBundle 用凭据束做一次侧车登录。
func ReloginWithBundle(ctx context.Context, b *CredentialBundle, progress LoginProgress) (*LoginResult, error) {
	if !b.CanRelogin() {
		return nil, fmt.Errorf("prism: bundle has no email/password")
	}
	timeout := 240
	return RunSidecarLogin(ctx, LoginRequest{
		Email:           b.Email,
		Password:        b.Password,
		TOTPSecret:      b.TOTP,
		Proxy:           b.Proxy,
		Backend:         b.Backend,
		Headless:        b.Headless,
		Timeout:         timeout,
		StorageStateIn:  b.StatePath,
		StorageStateOut: b.StatePath,
	}, progress)
}

var _ = time.Second
