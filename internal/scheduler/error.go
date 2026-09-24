package scheduler

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"
)

// UnavailableError 对应 CPA selector.go modelCooldownError：全部凭据冷却时 429 + Retry-After。
type UnavailableError struct {
	Model   string
	ResetIn time.Duration
	Message string
}

func (e *UnavailableError) Error() string {
	if e == nil {
		return "no auth available"
	}
	if e.Message != "" {
		return e.Message
	}
	modelName := e.Model
	if modelName == "" {
		modelName = "requested model"
	}
	return fmt.Sprintf("All credentials for model %s are cooling down", modelName)
}

func (e *UnavailableError) StatusCode() int {
	return http.StatusTooManyRequests
}

func (e *UnavailableError) RetryAfterSeconds() int {
	if e == nil || e.ResetIn <= 0 {
		return 0
	}
	sec := int(math.Ceil(e.ResetIn.Seconds()))
	if sec < 0 {
		return 0
	}
	return sec
}

// Headers 对应 CPA modelCooldownError.Headers。
func (e *UnavailableError) Headers() http.Header {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	if sec := e.RetryAfterSeconds(); sec > 0 {
		h.Set("Retry-After", strconv.Itoa(sec))
	}
	return h
}

func newUnavailable(model string, resetIn time.Duration, allCooling bool) error {
	if allCooling && resetIn > 0 {
		return &UnavailableError{Model: model, ResetIn: resetIn}
	}
	if allCooling {
		return &UnavailableError{Model: model, Message: "no auth available"}
	}
	return fmt.Errorf("no available accounts (all not logged in or temporarily disabled)")
}
