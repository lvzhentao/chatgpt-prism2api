package emulation

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const cachePrefixLookback = 10

// 大 prompt 指纹截断：逐块规范化序列化+链式哈希的 CPU 随 prompt 规模线性增长，
// 高 RPM 下大 prompt（几十万 token）会吃满核。总 token 超 cacheFastPathTokens 时只对
// 前缀块（累计 ≤ cacheFastPathPrefixTokens）做指纹，尾块不进 profile；token 记账经
// scaleBreakpointsToInputTokens 按比例放大到全量，缓存命中率的对外观感不变。
const (
	cacheFastPathTokens       = 100_000
	cacheFastPathPrefixTokens = 50_000
)

const (
	tokensPerMessage = 4
	tokensPerTool    = 150
	imageTokenFlat   = 1600
)

// Usage 是一次请求的缓存拆分。不变量：Input + Read + Creation = 请求总量。
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheCreation5m          int `json:"ephemeral_5m_input_tokens"`
	CacheCreation1h          int `json:"ephemeral_1h_input_tokens"`
}

func (u *Usage) total() int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// CacheLog 是最近一次缓存估算，供管理页调参。
type CacheLog struct {
	Time     string `json:"time"`
	Model    string `json:"model"`
	Total    int    `json:"total"`
	Input    int    `json:"input"`
	Read     int    `json:"read"`
	Creation int    `json:"creation"`
	Hit      bool   `json:"hit"`
}

type cacheEntry struct {
	tokens    int
	ttl       time.Duration
	expiresAt time.Time
}

// Tracker 按命名空间记住前缀指纹。
type Tracker struct {
	mu      sync.Mutex
	entries map[uint64]map[[32]byte]cacheEntry
}

func NewTracker() *Tracker {
	return &Tracker{entries: map[uint64]map[[32]byte]cacheEntry{}}
}

// Plan 先估算、成功后再 commit，避免失败请求污染前缀。
type Plan struct {
	Usage    *Usage
	cacheKey uint64
	profile  *cacheProfile
	tracker  *Tracker
}

func (p *Plan) Result() *Usage {
	if p == nil {
		return nil
	}
	return p.Usage
}

func (p *Plan) Commit() {
	if p == nil || p.profile == nil || p.cacheKey == 0 || p.tracker == nil {
		return
	}
	p.tracker.update(p.cacheKey, p.profile)
}

type cacheProfile struct {
	totalInputTokens              int
	minCacheable                  int
	scaleBreakpointsToInputTokens bool
	blocks                        []cacheBlock
	breakpoints                   []cacheBreakpoint
}

type cacheBlock struct {
	prefixFingerprint [32]byte
	cumulativeTokens  int
}

type cacheBreakpoint struct {
	blockIndex int
	ttl        time.Duration
}

type resolvedBreakpoint struct {
	blockIndex       int
	cumulativeTokens int
	ttl              time.Duration
}

type pendingBlock struct {
	value         any
	tokens        int
	breakpointTTL *time.Duration
	messageIndex  *int
	isMessageEnd  bool
}

// Namespace 把身份映射为 tracker 一级键。
func Namespace(keyID uint64, apiKey string) uint64 {
	if keyID > 0 {
		return keyID
	}
	if apiKey != "" {
		h := fnv.New64a()
		_, _ = h.Write([]byte(apiKey))
		return h.Sum64()
	}
	return 1
}

func (t *Tracker) prepare(cfg CacheConfig, ns uint64, body []byte, model string, inputTokens int) *Plan {
	if t == nil || !cfg.Enabled || len(body) == 0 || ns == 0 {
		return nil
	}
	profile, ok := buildCacheProfile(cfg, body, model, inputTokens)
	if !ok {
		return nil
	}
	result := t.compute(ns, profile)
	if cfg.ForceHit {
		result = forcedReadUsage(profile)
	}
	applyCacheRatios(result, cfg, inputTokens)
	if result.CacheReadInputTokens == 0 && result.CacheCreationInputTokens == 0 {
		return nil
	}
	return &Plan{Usage: result, cacheKey: ns, profile: profile, tracker: t}
}

// forcedReadUsage 跳过前缀匹配，把整段可缓存前缀直接记为 read（强制命中）。
func forcedReadUsage(profile *cacheProfile) *Usage {
	out := &Usage{}
	if profile == nil {
		return out
	}
	last := profile.lastCacheable()
	if last == nil {
		return out
	}
	out.CacheReadInputTokens = max(profile.tokensFor(last.cumulativeTokens), 0)
	return out
}

func applyCacheRatios(result *Usage, cfg CacheConfig, inputTokens int) {
	if result == nil {
		return
	}
	if cfg.Mode == ModeIndependent {
		creation, read := cfg.creationRatio(), cfg.sampleReadRatio()
		result.CacheReadInputTokens = scaleTokens(result.CacheReadInputTokens, read)
		result.CacheCreationInputTokens = scaleTokens(result.CacheCreationInputTokens, creation)
		result.CacheCreation5m, result.CacheCreation1h = scaleCreationTTL(
			result.CacheCreation5m, result.CacheCreation1h, result.CacheCreationInputTokens, creation,
		)
	} else {
		ratio := cfg.sampleUniformRatio()
		result.CacheReadInputTokens = scaleTokens(result.CacheReadInputTokens, ratio)
		result.CacheCreationInputTokens = scaleTokens(result.CacheCreationInputTokens, ratio)
		result.CacheCreation5m = scaleTokens(result.CacheCreation5m, ratio)
		result.CacheCreation1h = scaleTokens(result.CacheCreation1h, ratio)
	}
	result.InputTokens = inputTokens - result.CacheReadInputTokens - result.CacheCreationInputTokens
	if result.InputTokens < 0 {
		result.InputTokens = 0
	}
	reserveUncachedTail(result, inputTokens)
}

// reserveUncachedTail 留一小段未命中 input，避免把整段 prompt 记成 cache。
func reserveUncachedTail(result *Usage, total int) {
	if result == nil || total <= 0 {
		return
	}
	tail := 2
	if total < 16 {
		tail = max(total/4, 1)
	}
	if result.InputTokens >= tail {
		return
	}
	need := tail - result.InputTokens
	stolen := stealCacheTokens(&result.CacheCreationInputTokens, need)
	if stolen > 0 {
		shrinkCreationBuckets(result, stolen)
		need -= stolen
	}
	if need > 0 {
		_ = stealCacheTokens(&result.CacheReadInputTokens, need)
	}
	result.InputTokens = total - result.CacheReadInputTokens - result.CacheCreationInputTokens
	if result.InputTokens < 0 {
		result.InputTokens = 0
	}
}

func stealCacheTokens(src *int, need int) int {
	if src == nil || *src <= 0 || need <= 0 {
		return 0
	}
	take := need
	if take > *src {
		take = *src
	}
	*src -= take
	return take
}

func shrinkCreationBuckets(result *Usage, stolen int) {
	if result == nil || stolen <= 0 {
		return
	}
	if result.CacheCreation5m > 0 {
		d := min(result.CacheCreation5m, stolen)
		result.CacheCreation5m -= d
		stolen -= d
	}
	if stolen > 0 && result.CacheCreation1h > 0 {
		result.CacheCreation1h -= min(result.CacheCreation1h, stolen)
	}
}

func scaleTokens(tokens int, ratio float64) int {
	if tokens <= 0 || ratio <= 0 {
		return 0
	}
	if ratio >= 1 {
		return tokens
	}
	return int(math.Round(float64(tokens) * ratio))
}

func scaleCreationTTL(tokens5m, tokens1h, scaledTotal int, ratio float64) (int, int) {
	if scaledTotal <= 0 || ratio <= 0 {
		return 0, 0
	}
	if tokens1h <= 0 {
		return scaledTotal, 0
	}
	if tokens5m <= 0 {
		return 0, scaledTotal
	}
	scaled5m := scaleTokens(tokens5m, ratio)
	if scaled5m > scaledTotal {
		scaled5m = scaledTotal
	}
	return scaled5m, scaledTotal - scaled5m
}

func buildCacheProfile(cfg CacheConfig, body []byte, model string, inputTokens int) (*cacheProfile, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	blocks := flattenCacheBlocks(payload)
	if len(blocks) == 0 {
		return nil, false
	}
	total := inputTokens
	if total <= 0 {
		total = estimatePayloadTokens(payload)
	}
	if total > cacheFastPathTokens {
		blocks = truncateCacheBlocks(blocks, cacheFastPathPrefixTokens)
		if len(blocks) == 0 {
			return nil, false
		}
	}
	if !hasClientBreakpoint(blocks) && cfg.FallbackBreakpoints {
		applyDefaultBreakpoints(blocks, cfg.ttl5m())
	}
	prelude := map[string]any{"model": payload["model"], "tool_choice": payload["tool_choice"]}
	minCacheable := cfg.minTokensFor(model)
	// 检测站（HVOY cachecheck）会给 Opus 打 cache_control，但 tool schema 被
	// 网关剥掉后累计 token 常 < 4096。客户端显式要缓存时按官方外形记账，不再用 Opus 门槛拒掉。
	if hasClientBreakpoint(blocks) {
		minCacheable = 0
	}
	profile, ok := profileFromBlocks(total, prelude, blocks, minCacheable)
	if ok {
		profile.scaleBreakpointsToInputTokens = true
	}
	return profile, ok
}

func flattenCacheBlocks(payload map[string]any) []pendingBlock {
	var blocks []pendingBlock
	if tools, ok := payload["tools"].([]any); ok {
		for i, tool := range tools {
			value := stripCacheControl(tool)
			blocks = append(blocks, pendingBlock{
				value:         map[string]any{"kind": "tool", "tool_index": i, "tool": value},
				tokens:        countSerializedTokens(value),
				breakpointTTL: extractCacheTTL(tool),
			})
		}
	}
	for i, sys := range normalizeSystemBlocks(payload["system"]) {
		value := stripCacheControl(sys)
		canonicalizeSystemBlock(value)
		blocks = append(blocks, pendingBlock{
			value:         map[string]any{"kind": "system", "system_index": i, "block": value},
			tokens:        countSystemTokens(sys),
			breakpointTTL: extractCacheTTL(sys),
		})
	}
	messages, _ := payload["messages"].([]any)
	for mi, raw := range messages {
		msg, _ := raw.(map[string]any)
		role, _ := msg["role"].(string)
		switch content := msg["content"].(type) {
		case string:
			idx := mi
			block := map[string]any{"type": "text", "text": content}
			blocks = append(blocks, pendingBlock{
				value:        map[string]any{"kind": "message", "message_index": mi, "role": role, "block_index": 0, "block": block},
				tokens:       countValueTokens(block),
				messageIndex: &idx,
				isMessageEnd: true,
			})
		case []any:
			last := len(content) - 1
			for bi, rawBlock := range content {
				idx := mi
				value := stripCacheControl(rawBlock)
				blocks = append(blocks, pendingBlock{
					value:         map[string]any{"kind": "message", "message_index": mi, "role": role, "block_index": bi, "block": value},
					tokens:        countValueTokens(rawBlock),
					breakpointTTL: extractCacheTTL(rawBlock),
					messageIndex:  &idx,
					isMessageEnd:  bi == last,
				})
			}
		}
	}
	return blocks
}

func profileFromBlocks(totalTokens int, prelude any, blocks []pendingBlock, minCacheable int) (*cacheProfile, bool) {
	preludeJSON, err := canonicalJSON(prelude)
	if err != nil {
		return nil, false
	}
	prefixState := make([]byte, 8+len(preludeJSON))
	binary.BigEndian.PutUint64(prefixState[:8], uint64(len(preludeJSON)))
	copy(prefixState[8:], preludeJSON)

	profile := &cacheProfile{totalInputTokens: max(totalTokens, 0), minCacheable: minCacheable}
	cumulative := 0
	var activeTTL *time.Duration
	seen := map[int]struct{}{}
	for i, block := range blocks {
		cumulative += max(block.tokens, 0)
		blockJSON, err := canonicalJSON(block.value)
		if err != nil {
			return nil, false
		}
		blockHash := sha256.Sum256(blockJSON)
		h := sha256.New()
		_, _ = h.Write(prefixState)
		_, _ = h.Write(blockHash[:])
		fp := [32]byte(h.Sum(nil))
		prefixState = fp[:]
		profile.blocks = append(profile.blocks, cacheBlock{prefixFingerprint: fp, cumulativeTokens: cumulative})
		if block.breakpointTTL != nil {
			ttl := *block.breakpointTTL
			activeTTL = &ttl
			if _, ok := seen[i]; !ok {
				profile.breakpoints = append(profile.breakpoints, cacheBreakpoint{blockIndex: i, ttl: ttl})
				seen[i] = struct{}{}
			}
		}
		if block.isMessageEnd && block.messageIndex != nil && activeTTL != nil {
			if _, ok := seen[i]; !ok {
				profile.breakpoints = append(profile.breakpoints, cacheBreakpoint{blockIndex: i, ttl: *activeTTL})
				seen[i] = struct{}{}
			}
		}
	}
	if profile.lastCacheable() == nil {
		return nil, false
	}
	return profile, true
}

func (p *cacheProfile) cacheable() []resolvedBreakpoint {
	if p == nil {
		return nil
	}
	var out []resolvedBreakpoint
	for _, bp := range p.breakpoints {
		if bp.blockIndex < 0 || bp.blockIndex >= len(p.blocks) {
			continue
		}
		tok := p.blocks[bp.blockIndex].cumulativeTokens
		if tok < p.minCacheable {
			continue
		}
		out = append(out, resolvedBreakpoint{blockIndex: bp.blockIndex, cumulativeTokens: tok, ttl: bp.ttl})
	}
	return out
}

func (p *cacheProfile) lastCacheable() *resolvedBreakpoint {
	all := p.cacheable()
	if len(all) == 0 {
		return nil
	}
	last := all[len(all)-1]
	return &last
}

func (p *cacheProfile) tokensFor(cumulative int) int {
	if p == nil {
		return 0
	}
	if !p.scaleBreakpointsToInputTokens {
		return min(max(cumulative, 0), p.totalInputTokens)
	}
	last := p.lastCacheable()
	if last == nil || last.cumulativeTokens <= 0 {
		return min(max(cumulative, 0), p.totalInputTokens)
	}
	scaled := int(math.Round(float64(max(cumulative, 0)) * float64(p.totalInputTokens) / float64(last.cumulativeTokens)))
	return min(max(scaled, 0), p.totalInputTokens)
}

func (t *Tracker) compute(ns uint64, profile *cacheProfile) *Usage {
	out := &Usage{}
	if t == nil || profile == nil || ns == 0 {
		return out
	}
	last := profile.lastCacheable()
	if last == nil {
		return out
	}
	lastTokens := profile.tokensFor(last.cumulativeTokens)
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)

	matched := 0
	if entries := t.entries[ns]; entries != nil {
		bps := profile.cacheable()
		for i, seen := len(bps)-1, 0; i >= 0 && seen < cachePrefixLookback; i, seen = i-1, seen+1 {
			bp := bps[i]
			candidate := profile.blocks[bp.blockIndex]
			entry, ok := entries[candidate.prefixFingerprint]
			if !ok || !entry.expiresAt.After(now) {
				continue
			}
			entry.expiresAt = now.Add(entry.ttl)
			entries[candidate.prefixFingerprint] = entry
			matched = profile.tokensFor(bp.cumulativeTokens)
			break
		}
	}
	out.CacheReadInputTokens = max(matched, 0)
	out.CacheCreationInputTokens = max(lastTokens-matched, 0)
	if out.CacheCreationInputTokens > 0 {
		if last.ttl >= time.Hour {
			out.CacheCreation1h = out.CacheCreationInputTokens
		} else {
			out.CacheCreation5m = out.CacheCreationInputTokens
		}
	}
	return out
}

func (t *Tracker) update(ns uint64, profile *cacheProfile) {
	if t == nil || profile == nil || ns == 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)
	entries := t.entries[ns]
	if entries == nil {
		entries = map[[32]byte]cacheEntry{}
		t.entries[ns] = entries
	}
	for _, bp := range profile.cacheable() {
		block := profile.blocks[bp.blockIndex]
		exp := now.Add(bp.ttl)
		if entry, ok := entries[block.prefixFingerprint]; ok {
			if block.cumulativeTokens > entry.tokens {
				entry.tokens = block.cumulativeTokens
			}
			if bp.ttl > entry.ttl {
				entry.ttl = bp.ttl
			}
			if exp.After(entry.expiresAt) {
				entry.expiresAt = exp
			}
			entries[block.prefixFingerprint] = entry
			continue
		}
		entries[block.prefixFingerprint] = cacheEntry{tokens: block.cumulativeTokens, ttl: bp.ttl, expiresAt: exp}
	}
}

func (t *Tracker) pruneLocked(now time.Time) {
	for ns, entries := range t.entries {
		for fp, entry := range entries {
			if !entry.expiresAt.After(now) {
				delete(entries, fp)
			}
		}
		if len(entries) == 0 {
			delete(t.entries, ns)
		}
	}
}

// truncateCacheBlocks 保留累计 token ≤ maxTokens 的前缀块（首块超限也保留 1 块，保证可缓存）。
func truncateCacheBlocks(blocks []pendingBlock, maxTokens int) []pendingBlock {
	cum := 0
	for i, b := range blocks {
		cum += max(b.tokens, 0)
		if cum > maxTokens && i > 0 {
			return blocks[:i]
		}
	}
	return blocks
}

func hasClientBreakpoint(blocks []pendingBlock) bool {
	for _, b := range blocks {
		if b.breakpointTTL != nil {
			return true
		}
	}
	return false
}

func applyDefaultBreakpoints(blocks []pendingBlock, ttl time.Duration) {
	if len(blocks) == 0 {
		return
	}
	for i := range blocks {
		if blocks[i].isMessageEnd {
			blocks[i].breakpointTTL = &ttl
		}
	}
	blocks[len(blocks)-1].breakpointTTL = &ttl
}

func stripCacheControl(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			if k == "cache_control" {
				continue
			}
			out[k] = stripCacheControl(child)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = stripCacheControl(child)
		}
		return out
	default:
		return v
	}
}

func canonicalizeSystemBlock(value any) {
	obj, ok := value.(map[string]any)
	if !ok {
		return
	}
	text, _ := obj["text"].(string)
	if strings.HasPrefix(text, "x-anthropic-billing-header:") {
		obj["text"] = "__anthropic_billing_header__"
	}
}

func extractCacheTTL(v any) *time.Duration {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	cc, ok := m["cache_control"].(map[string]any)
	if !ok {
		return nil
	}
	if typ, _ := cc["type"].(string); typ != "" && !strings.EqualFold(typ, "ephemeral") {
		return nil
	}
	ttl := 5 * time.Minute
	if s, _ := cc["ttl"].(string); strings.EqualFold(strings.TrimSpace(s), "1h") {
		ttl = time.Hour
	}
	if ttl > time.Hour {
		ttl = time.Hour
	}
	return &ttl
}

func normalizeSystemBlocks(v any) []any {
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": t}}
	case []any:
		return t
	default:
		return nil
	}
}

func countValueTokens(v any) int {
	switch t := v.(type) {
	case string:
		return estimateTextTokens(t)
	case []any:
		n := 0
		for _, item := range t {
			n += countValueTokens(item)
		}
		return n
	case map[string]any:
		if typ, _ := t["type"].(string); typ == "image" || typ == "image_url" || typ == "input_image" {
			return imageTokenFlat
		}
		if typ, _ := t["type"].(string); typ == "document" {
			return countDocumentTokens(t)
		}
		n := 0
		if s, ok := t["text"].(string); ok {
			n += estimateTextTokens(s)
		}
		if s, ok := t["thinking"].(string); ok {
			n += estimateTextTokens(s)
		}
		if raw, ok := t["input"]; ok {
			n += countSerializedTokens(raw)
		}
		if content, ok := t["content"]; ok && n == 0 {
			n += countValueTokens(content)
		}
		return n
	default:
		return countSerializedTokens(v)
	}
}

func countSystemTokens(v any) int {
	switch t := v.(type) {
	case string:
		return estimateTextTokens(t)
	case map[string]any:
		if s, ok := t["text"].(string); ok {
			return estimateTextTokens(s)
		}
	}
	return 0
}

func countDocumentTokens(m map[string]any) int {
	src, _ := m["source"].(map[string]any)
	if src == nil {
		return countSerializedTokens(m)
	}
	media, _ := src["media_type"].(string)
	typ, _ := src["type"].(string)
	data, _ := src["data"].(string)
	url, _ := src["url"].(string)
	title, _ := m["title"].(string)
	text := ExtractDocument(media, typ, data, url, title)
	if text != "" {
		return estimateTextTokens(text)
	}
	return countSerializedTokens(m)
}

func countSerializedTokens(v any) int {
	raw, err := canonicalJSON(v)
	if err != nil {
		return 0
	}
	return estimateTextTokens(string(raw))
}

func estimatePayloadTokens(payload map[string]any) int {
	n := 0
	for _, block := range normalizeSystemBlocks(payload["system"]) {
		n += countSystemTokens(block)
	}
	if messages, ok := payload["messages"].([]any); ok {
		n += countValueTokens(messages)
		n += len(messages) * tokensPerMessage
	}
	if tools, ok := payload["tools"].([]any); ok {
		n += len(tools) * tokensPerTool
		for _, tool := range tools {
			n += countSerializedTokens(stripCacheControl(tool))
		}
	}
	if n < 1 {
		return 1
	}
	return n
}

func canonicalJSON(v any) ([]byte, error) {
	var buf strings.Builder
	if err := writeCanonicalJSON(&buf, v); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

func writeCanonicalJSON(buf *strings.Builder, v any) error {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonicalJSON(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case []any:
		buf.WriteByte('[')
		for i, child := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalJSON(buf, child); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(b)
		return nil
	}
}

func estimateTextTokens(s string) int {
	ascii, wide := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			wide++
		}
	}
	n := ascii/4 + wide
	if n < 1 && s != "" {
		return 1
	}
	return n
}
