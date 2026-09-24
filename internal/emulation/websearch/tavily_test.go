package websearch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchTavily(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) == "" {
			t.Fatal("empty body")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"ok","results":[{"title":"T","url":"https://example.com","content":"snippet"}]}`))
	}))
	defer srv.Close()
	oldURL, oldHTTP := TavilyEndpoint, HTTPClient
	TavilyEndpoint = srv.URL
	HTTPClient = srv.Client()
	defer func() { TavilyEndpoint, HTTPClient = oldURL, oldHTTP }()

	resp, err := SearchTavily(context.Background(), "tvly-test", "hello", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://example.com" {
		t.Fatalf("%+v", resp)
	}
}
