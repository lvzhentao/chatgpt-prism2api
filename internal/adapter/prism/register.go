package prism

import (
	"context"

	"prism-2api/internal/adapter"
)

// ============================================================
// 可选 — 自动注册。不需要就保持 PathSignup 带 TODO，账号页导入。
// ============================================================

type StepLog func(msg string)

func (a *Adapter) Register(ctx context.Context, log StepLog) error {
	_ = ctx
	if stringsContainsTODO(PathSignup) {
		return adapter.ErrNotImplemented
	}
	if log != nil {
		log("TODO: implement Register")
	}
	return adapter.ErrNotImplemented
}

func stringsContainsTODO(s string) bool {
	return len(s) >= 4 && (s == "/v1/TODO/signup" || containsTODO(s))
}

func containsTODO(s string) bool {
	for i := 0; i+4 <= len(s); i++ {
		if s[i:i+4] == "TODO" {
			return true
		}
	}
	return false
}
