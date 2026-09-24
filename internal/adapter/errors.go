package adapter

import "errors"

// ErrNotImplemented is returned by site TODOs that have not been filled yet.
var ErrNotImplemented = errors.New("adapter: not implemented")

// AuthError is an invalid / expired credential.
type AuthError struct{ Msg string }

func (e *AuthError) Error() string { return e.Msg }

// InsufficientCredits is a quota-exhausted upstream response.
type InsufficientCredits struct{ Msg string }

func (e *InsufficientCredits) Error() string { return e.Msg }

// UpstreamTemporary is a retryable upstream failure.
type UpstreamTemporary struct{ Msg string }

func (e *UpstreamTemporary) Error() string { return e.Msg }
