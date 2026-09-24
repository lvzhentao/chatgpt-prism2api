package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"prism-2api/internal/emulation"
)

func (s *Server) handleAdminEmulation(w http.ResponseWriter, r *http.Request) {
	if s.emu == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "emulation store not ready"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg := s.emu.Config()
		first, second := cfg.Cache.Preview(20000)
		writeJSON(w, http.StatusOK, map[string]any{
			"config":     cfg.PublicView(),
			"cache_log":  s.emu.CacheLogs(),
			"search_log": s.emu.SearchLogs(),
			"preview":    map[string]any{"example_input": 20000, "first": first, "second": second},
		})
	case http.MethodPut:
		var next emulation.Config
		if err := json.NewDecoder(ioLimit(r.Body)).Decode(&next); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
			return
		}
		cfg, err := s.emu.Update(next)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"config":     cfg,
			"cache_log":  s.emu.CacheLogs(),
			"search_log": s.emu.SearchLogs(),
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use GET or PUT"})
	}
}

func (s *Server) handleAdminEmulationSearchTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use POST"})
		return
	}
	var req struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(ioLimit(r.Body)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "query is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	start := time.Now()
	resp, provider, err := s.searchWeb(ctx, query)
	latency := time.Since(start).Milliseconds()
	if s.emu != nil {
		log := emulation.SearchLog{Query: truncateRunes(query, 160), Provider: provider, LatencyMS: latency}
		if err != nil {
			log.Error = err.Error()
		} else if resp != nil {
			log.Results = len(resp.Results)
		}
		s.emu.NoteSearch(log)
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "provider": provider, "error": err.Error(), "latency_ms": latency,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "provider": provider, "results": resp.Results, "answer": resp.Answer, "latency_ms": latency,
	})
}
