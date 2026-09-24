package websearch

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	mdLinkRe = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)\s]+)\)(?:\s*[:：—-]\s*(.*))?`)
	bareURL  = regexp.MustCompile(`https?://[^\s\)\]>\"']+`)
)

// ParseMarkdownResults 从 Cursor WebSearch 的 markdown 回复里抽出结果。
func ParseMarkdownResults(text string, max int) []Result {
	if max <= 0 {
		max = 5
	}
	var out []Result
	seen := map[string]bool{}
	add := func(title, rawURL, snippet string) {
		rawURL = strings.TrimRight(rawURL, ".,;)]}")
		if rawURL == "" || seen[rawURL] {
			return
		}
		seen[rawURL] = true
		title = strings.TrimSpace(title)
		if title == "" {
			title = hostname(rawURL)
		}
		out = append(out, Result{Title: title, URL: rawURL, Snippet: strings.TrimSpace(snippet)})
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := mdLinkRe.FindStringSubmatch(line); len(m) >= 3 {
			add(m[1], m[2], "")
			if len(m) >= 4 {
				out[len(out)-1].Snippet = strings.TrimSpace(m[3])
			}
			if len(out) >= max {
				return out
			}
			continue
		}
	}
	if len(out) == 0 {
		for _, m := range bareURL.FindAllString(text, max) {
			add("", m, "")
			if len(out) >= max {
				break
			}
		}
	}
	if len(out) > max {
		return out[:max]
	}
	return out
}

func hostname(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}
