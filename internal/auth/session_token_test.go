package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func sampleJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix(), "sub": "auth0|user_01TEST"})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestExtractAccessToken(t *testing.T) {
	tok := sampleJWT(t, time.Now().Add(time.Hour))
	cases := []struct {
		name string
		in   string
	}{
		{"raw", tok},
		{"session", "user_01TEST::" + tok},
		{"urlencoded", "user_01TEST%3A%3A" + tok},
		{"cookie", "WorkosCursorSessionToken=user_01TEST%3A%3A" + tok},
		{"dash_line", "a@b.com----pw----x----user_01TEST%3A%3A" + tok},
	}
	for _, tc := range cases {
		got := ExtractAccessToken(tc.in)
		if got != tok {
			t.Fatalf("%s: got %q want jwt", tc.name, got)
		}
	}
	if ExtractAccessToken("not-a-token") != "" {
		t.Fatal("expected empty")
	}
}

func TestWorkOSUserID(t *testing.T) {
	tok := sampleJWT(t, time.Now().Add(time.Hour))
	if got := WorkOSUserID(tok); got != "user_01TEST" {
		t.Fatalf("got %q", got)
	}
	if WorkOSUserID("not-a-jwt") != "" {
		t.Fatal("expected empty")
	}
}
