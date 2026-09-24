package api

import (
	"net/http"

	"prism-2api/internal/failclass"
	"prism-2api/internal/pool"
)

// noteUpstream 用 CPA/kiro 分类结果更新账号冷却。clientGone 时一律当取消：不冷却、不换号。
func noteUpstream(acc *pool.Account, err error, model string, clientGone bool) failclass.Result {
	if clientGone {
		return failclass.Result{
			Class:      failclass.Canceled,
			Cool:       false,
			Switch:     false,
			HTTPStatus: failclass.StatusClientClosedRequest,
		}
	}
	res := failclass.Classify(err)
	acc.MarkFailure(res, model)
	return res
}

func openaiErrorType(c failclass.Class) string {
	switch c {
	case failclass.BadRequest, failclass.Region:
		return "invalid_request_error"
	case failclass.RateLimit:
		return "rate_limit_error"
	case failclass.Auth:
		return "authentication_error"
	default:
		return "upstream_error"
	}
}

func writeOpenAIClassError(w http.ResponseWriter, res failclass.Result, msg string) {
	status := res.HTTPStatus
	if status <= 0 {
		status = http.StatusBadGateway
	}
	setRetryAfter(w, res.Class)
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    openaiErrorType(res.Class),
			"code":    officialErrorCode(res.Class),
		},
	})
}

func anthropicErrorType(c failclass.Class) string {
	switch c {
	case failclass.BadRequest, failclass.Region:
		return "invalid_request_error"
	case failclass.RateLimit:
		return "rate_limit_error"
	case failclass.Auth:
		return "authentication_error"
	default:
		return "api_error"
	}
}
