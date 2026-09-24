package websearch

// Result 是一条可映射为官方 web_search_result 的结果。
type Result struct {
	Title   string
	URL     string
	Snippet string
	PageAge string
}

// SearchResponse 是一次搜索的聚合结果。
type SearchResponse struct {
	Results  []Result
	Answer   string
	Provider string
}
