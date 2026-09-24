package auth

import "net/http"

// ExchangeHook is bound at boot to adapter.Default.Auth().ExchangeCredential.
// TokenManager.Refresh calls this instead of a hardcoded vendor endpoint.
var ExchangeHook = func(apiBaseURL, secret, clientVersion, clientType string, client *http.Client) (*Token, error) {
	return nil, ErrInvalidAPIKey
}

// LoginURLHook builds the browser login URL. Empty means "API key import only".
var LoginURLHook = func(websiteURL, challenge, uuid string) string {
	return ""
}

// PollHook waits for a browser login to finish.
var PollHook = func(apiBaseURL, uuid, verifier, clientVersion, clientType string, client *http.Client) (*Token, error) {
	return nil, ErrInvalidAPIKey
}

// IdentitySubject extracts a stable user id from an access token (JWT sub by default).
func IdentitySubject(accessToken string) string {
	return JWTSubject(accessToken)
}
