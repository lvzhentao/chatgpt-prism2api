package emulation

import (
	"context"
	"fmt"
	"sync/atomic"

	"prism-2api/internal/emulation/websearch"
	"prism-2api/internal/secret"
)

// SearchTavilyPool 轮询 Tavily key 池，失败自动换下一把。
func (s *Store) SearchTavilyPool(ctx context.Context, query string, maxResults int) (*websearch.SearchResponse, error) {
	keys := s.enabledTavilyKeys()
	if len(keys) == 0 {
		return nil, fmt.Errorf("tavily: key pool is empty")
	}
	start := int(atomic.AddUint64(&s.tavilyRR, 1)-1) % len(keys)
	var last error
	for i := 0; i < len(keys); i++ {
		k := keys[(start+i)%len(keys)]
		plain := secret.Open(k.Key)
		resp, err := websearch.SearchTavily(ctx, plain, query, maxResults)
		if err != nil {
			s.markTavily(k.ID, err.Error())
			last = err
			continue
		}
		s.markTavily(k.ID, "")
		if resp != nil && resp.Provider == "" {
			resp.Provider = "tavily"
		}
		return resp, nil
	}
	if last == nil {
		last = fmt.Errorf("tavily: all keys failed")
	}
	return nil, last
}
