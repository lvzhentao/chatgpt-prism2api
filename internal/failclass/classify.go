package failclass

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// CPA internal/clienterror request-fault identifiers.
var requestFaultCodes = map[string]struct{}{
	"cyber_policy":                {},
	"context_length_exceeded":     {},
	"message_too_big":             {},
	"string_above_max_length":     {},
	"invalid_prompt":              {},
	"invalid_value":               {},
	"unsupported_value":           {},
	"invalid_request_error":       {},
	"previous_response_not_found": {},
}

var requestFaultTypes = map[string]struct{}{
	"invalid_request":       {},
	"invalid_request_error": {},
	"bad_request_error":     {},
	"invalid_prompt":        {},
}

var (
	httpStatusRe = regexp.MustCompile(`(?i)(?:http|status)\s+(\d{3})\b`)
)

type statusCoder interface {
	StatusCode() int
}

// HTTPStatusFromError 移植自 CPA clienterror.HTTPStatusFromError。
// StatusCode() 优先；否则 Canceled→499、DeadlineExceeded→504。
// 额外从 Cursor 包装文案里解析 "http 429" / "status 502"（本仓库 client 尚未全部带 StatusCode）。
func HTTPStatusFromError(err error) int {
	if err == nil {
		return 0
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		if code := sc.StatusCode(); code > 0 {
			return code
		}
	}
	if errors.Is(err, context.Canceled) {
		return StatusClientClosedRequest
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	if m := httpStatusRe.FindStringSubmatch(err.Error()); len(m) == 2 {
		n := 0
		for _, c := range m[1] {
			n = n*10 + int(c-'0')
		}
		if n >= 100 && n <= 599 {
			return n
		}
	}
	return 0
}

// attachedStatus 只看 StatusCode()，与 CPA statusCodeFromError 一致。
// 生命周期判断用这个：DeadlineExceeded 没有 StatusCode，才能被当成连接生命周期。
func attachedStatus(err error) int {
	if err == nil {
		return 0
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		return sc.StatusCode()
	}
	return 0
}

// IsRequestFault 移植自 CPA clienterror.IsRequestFault。
// 400/409/413/422 或 JSON error.code/type 命中请求过错表。
// 429 与 402 即使 body 写 invalid_request_error 也不是请求过错。
func IsRequestFault(status int, err error) bool {
	if status <= 0 && err != nil {
		if code := attachedStatus(err); code > 0 {
			status = code
		}
	}
	if status == http.StatusPaymentRequired || status == http.StatusTooManyRequests {
		return false
	}
	if status == http.StatusUnauthorized && hasAuthenticationErrorBody(err) {
		return false
	}
	if hasRequestFaultBody(err) {
		return true
	}
	if err != nil && IsItemNotPersisted(err.Error()) {
		return true
	}
	switch status {
	case http.StatusBadRequest,
		http.StatusConflict,
		http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// IsItemNotPersisted 移植自 CPA clienterror.IsItemNotPersisted。
func IsItemNotPersisted(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "item with id") &&
		strings.Contains(lower, "not found") &&
		strings.Contains(lower, "items are not persisted when `store` is set to false")
}

func hasAuthenticationErrorBody(err error) bool {
	body := errorJSONBody(err)
	if body == "" {
		return false
	}
	for _, path := range []string{"error.type", "type", "response.error.type", "body.error.type"} {
		if strings.ToLower(strings.TrimSpace(jsonStringAt(body, path))) == "authentication_error" {
			return true
		}
	}
	return false
}

func hasRequestFaultBody(err error) bool {
	body := errorJSONBody(err)
	if body == "" {
		return false
	}
	for _, path := range []string{"error.code", "code", "response.error.code", "body.error.code"} {
		code := strings.ToLower(strings.TrimSpace(jsonStringAt(body, path)))
		if _, ok := requestFaultCodes[code]; ok {
			return true
		}
	}
	for _, path := range []string{"error.type", "type", "response.error.type", "body.error.type"} {
		errType := strings.ToLower(strings.TrimSpace(jsonStringAt(body, path)))
		if _, ok := requestFaultTypes[errType]; ok {
			return true
		}
	}
	return false
}

func errorJSONBody(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	if s == "" {
		return ""
	}
	if json.Valid([]byte(s)) {
		return s
	}
	// Cursor 客户端常包装成 "agent run error: {...}"；抽出 JSON 再套 CPA 的 code/type 表。
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		sub := s[i : j+1]
		if json.Valid([]byte(sub)) {
			return sub
		}
	}
	return ""
}

func jsonStringAt(body, path string) string {
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return ""
	}
	cur := v
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = m[p]
		if !ok {
			return ""
		}
	}
	switch t := cur.(type) {
	case string:
		return t
	case float64:
		return strings.TrimSuffix(strings.TrimSuffix(jsonNumber(t), ".0"), ".")
	case json.Number:
		return t.String()
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// isDegradedMessage 识别适配器的「上游 backend 劣化」文案：预热预算耗尽时适配器报
// "prism: upstream sandbox degraded"（实测劣化期 /api/backend/1/new 24~210s 甚至挂死）。
// 只在文案层判断，是因为 failclass 只看得到 error。语义同上游维护：这是上游整体不可用，
// 不是账号问题 —— 换号打的是同一个 backend，冷却只会白关好账号。
func isDegradedMessage(msg string) bool {
	return strings.Contains(msg, "sandbox degraded")
}

// isConnectionLifecycle 移植自 CPA isConnectionLifecycleError：
// 客户端取消、超时、EOF 不冷却凭据；若已带 HTTP 状态则绝不按文案改判。

func isConnectionLifecycle(err error) bool {
	if err == nil {
		return false
	}
	if attachedStatus(err) != 0 {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return isConnectionLifecycleMessage(err.Error())
}

func isConnectionLifecycleMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	switch lower {
	case "context canceled", "context deadline exceeded", "eof", "unexpected eof":
		return true
	}
	if strings.Contains(lower, "websocket: close 1000") ||
		strings.Contains(lower, "websocket: close 1001") ||
		strings.Contains(lower, "websocket: close 1006") {
		return true
	}
	if strings.Contains(lower, "unexpected eof") {
		return true
	}
	return false
}

func result(class Class, cool, switchAcc bool, status int) Result {
	if status <= 0 {
		status = defaultHTTPStatus(class)
	}
	return Result{Class: class, Cool: cool, Switch: switchAcc, HTTPStatus: status}
}

func defaultHTTPStatus(c Class) int {
	switch c {
	case Canceled:
		return StatusClientClosedRequest
	case BadRequest, Region:
		return http.StatusBadRequest
	case RateLimit:
		return http.StatusTooManyRequests
	case Quota:
		return http.StatusPaymentRequired
	case Auth:
		return http.StatusUnauthorized
	case Unavailable:
		return http.StatusServiceUnavailable
	case Server:
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}

// Classify 把任意上游 error 映到 Result。
// 顺序：客户端取消 → 连接生命周期 → Cursor 地区/模型文案 → HTTP 状态（CPA conductor）→ 请求过错 → 默认 Server。
func Classify(err error) Result {
	if err == nil {
		return Result{Class: None}
	}

	status := HTTPStatusFromError(err)
	msg := strings.ToLower(err.Error())

	// 客户端取消：不冷却、不换号（本仓库 OpenAI 路径原注释 + CPA 499）。
	if errors.Is(err, context.Canceled) {
		return result(Canceled, false, false, StatusClientClosedRequest)
	}

	// 上游 backend 劣化（适配器预热预算耗尽）：同上游维护处理 —— 上游整体不可用，
	// 不是账号问题。冷却会把好账号白关几分钟，换号只是把别的账号也拖下水，
	// 所以两者都关掉，直接把 503 交给客户端。
	if isDegradedMessage(msg) {
		return result(Unavailable, false, false, http.StatusServiceUnavailable)
	}

	// CPA：生命周期失败跳过冷却，但不断 credential fallback（换号）。
	if isConnectionLifecycle(err) {
		st := status
		if st <= 0 {
			st = http.StatusGatewayTimeout
		}
		return result(Server, false, true, st)
	}

	// Cursor 地区限制：原 isAccountFatalError 非致命，计划表 Region 不冷却不换号。
	if strings.Contains(msg, "not supported in your region") {
		return result(Region, false, false, http.StatusBadRequest)
	}

	// 单模型暂时不可用：kiro ModelUnavailable，模型级 5 分钟。
	if strings.Contains(msg, "model not available") ||
		strings.Contains(msg, "model_temporarily_unavailable") ||
		strings.Contains(msg, "temporarily unavailable") {
		return result(Unavailable, true, true, http.StatusServiceUnavailable)
	}

	// Cursor 欠费/配额：ERROR_RATE_LIMITED | unpaid invoice 不带 HTTP 状态码，
	// 必须在状态码分支之前识别，否则落默认 Server（只冷却 2 分钟，欠费号反复出现）。
	if isQuotaMessage(msg) {
		return result(Quota, true, true, http.StatusPaymentRequired)
	}

	switch status {
	case http.StatusTooManyRequests:
		if isQuotaMessage(msg) {
			return result(Quota, true, true, status)
		}
		return result(RateLimit, true, true, status)
	case http.StatusPaymentRequired:
		return result(Quota, true, true, status)
	case http.StatusUnauthorized:
		return result(Auth, true, true, status)
	case http.StatusForbidden:
		if isQuotaMessage(msg) {
			return result(Quota, true, true, status)
		}
		return result(Auth, true, true, status)
	}

	// 请求过错先于 5xx：CPA IsRequestFault 在 500 上仍能识别 context_length_exceeded。
	if IsRequestFault(status, err) {
		return result(BadRequest, false, false, http.StatusBadRequest)
	}

	// 原 isAccountFatalError 非致命子串（Cursor 明文，不一定是 JSON）。
	if isCursorBadRequestMessage(msg) {
		return result(BadRequest, false, false, http.StatusBadRequest)
	}

	switch status {
	case http.StatusRequestTimeout, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return result(Server, true, true, status)
	case http.StatusNotFound:
		return result(Unavailable, true, true, status)
	}

	// CPA conductor default：可恢复失败。
	return result(Server, true, true, status)
}

func isQuotaMessage(msg string) bool {
	return strings.Contains(msg, "quota") &&
		(strings.Contains(msg, "exceed") || strings.Contains(msg, "exhaust") || strings.Contains(msg, "insufficient")) ||
		strings.Contains(msg, "insufficient balance") ||
		strings.Contains(msg, "payment required") ||
		// Cursor 欠费账号：ERROR_RATE_LIMITED | You have an unpaid invoice...
		strings.Contains(msg, "unpaid invoice") ||
		strings.Contains(msg, "unpaid_invoice")
}

func isCursorBadRequestMessage(msg string) bool {
	return strings.Contains(msg, "model not found") ||
		strings.Contains(msg, "error_bad_model_name") ||
		strings.Contains(msg, "not_valid") ||
		strings.Contains(msg, "invalid_request") ||
		strings.Contains(msg, "context_length_exceeded") ||
		strings.Contains(msg, "error_custom_message")
}
