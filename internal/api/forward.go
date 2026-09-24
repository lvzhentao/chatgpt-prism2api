package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
)

// internalChatRequest 构造内部 POST /v1/chat/completions，复用原请求的
// Context 与 Header，使 scheduler.ExtractSessionIDs 仍能看到
// X-Session-ID / Session-Id / Claude 元数据头。r 为 nil 时（batches）只带 body。
func internalChatRequest(r *http.Request, body []byte) *http.Request {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req = req.WithContext(ctx)
	if r != nil {
		if cloned := r.Header.Clone(); cloned != nil {
			req.Header = cloned
		}
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}
