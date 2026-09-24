package websearch

import (
	"fmt"
	"strings"
)

// TextSummary 收成官方那种短助手文本。
func TextSummary(query string, resp *SearchResponse) string {
	if resp == nil || len(resp.Results) == 0 {
		return "Based on the search results, I could not find reliable sources for: " + query
	}
	if ans := strings.TrimSpace(resp.Answer); ans != "" {
		return "Based on the search results, " + clipRunes(ans, 360)
	}
	titles := make([]string, 0, 3)
	for _, r := range resp.Results {
		if t := strings.TrimSpace(r.Title); t != "" {
			titles = append(titles, t)
		}
		if len(titles) >= 3 {
			break
		}
	}
	if len(titles) == 0 {
		return fmt.Sprintf("Based on the search results for %q.", query)
	}
	return clipRunes(fmt.Sprintf("Based on the search results, recent sources include %s.", strings.Join(titles, "; ")), 360)
}

func clipRunes(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	i := 0
	for idx := range s {
		if i == n {
			return strings.TrimSpace(s[:idx])
		}
		i++
	}
	return s
}
