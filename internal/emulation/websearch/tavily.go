package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TavilyEndpoint 可在测试中改写。
var (
	TavilyEndpoint = "https://api.tavily.com/search"
	HTTPClient     = &http.Client{Timeout: 20 * time.Second}
)

type tavilyRequest struct {
	APIKey        string `json:"api_key"`
	Query         string `json:"query"`
	MaxResults    int    `json:"max_results"`
	SearchDepth   string `json:"search_depth"`
	IncludeAnswer bool   `json:"include_answer"`
}

type tavilyResponse struct {
	Answer  string `json:"answer"`
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
	Detail string `json:"detail"`
	Error  string `json:"error"`
}

// SearchTavily 用单把 key 调 Tavily Search。
func SearchTavily(ctx context.Context, apiKey, query string, maxResults int) (*SearchResponse, error) {
	apiKey = strings.TrimSpace(apiKey)
	query = strings.TrimSpace(query)
	if apiKey == "" {
		return nil, fmt.Errorf("tavily: empty api key")
	}
	if query == "" {
		return nil, fmt.Errorf("tavily: empty query")
	}
	if maxResults <= 0 {
		maxResults = 5
	}
	body, err := json.Marshal(tavilyRequest{
		APIKey:        apiKey,
		Query:         query,
		MaxResults:    maxResults,
		SearchDepth:   "basic",
		IncludeAnswer: true,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TavilyEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tavily: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("tavily: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed tavilyResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("tavily: decode: %w", err)
	}
	if parsed.Error != "" {
		return nil, fmt.Errorf("tavily: %s", parsed.Error)
	}
	if parsed.Detail != "" && len(parsed.Results) == 0 {
		return nil, fmt.Errorf("tavily: %s", parsed.Detail)
	}
	out := &SearchResponse{Answer: parsed.Answer, Provider: "tavily"}
	for _, r := range parsed.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		out.Results = append(out.Results, Result{
			Title:   strings.TrimSpace(r.Title),
			URL:     strings.TrimSpace(r.URL),
			Snippet: strings.TrimSpace(r.Content),
		})
	}
	if len(out.Results) == 0 {
		return nil, fmt.Errorf("tavily: no results")
	}
	return out, nil
}
