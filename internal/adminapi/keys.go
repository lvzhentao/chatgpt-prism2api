package adminapi

import (
	"net/http"
)

// ListClientKeys GET /api/admin/client-keys
func (h *Handler) ListClientKeys(w http.ResponseWriter, r *http.Request) {
	if h.Keys == nil {
		writeNotReady(w)
		return
	}
	keys := h.Keys.List()
	if keys == nil {
		keys = []ClientKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys, "total": len(keys)})
}

// CreateClientKey POST /api/admin/client-keys
func (h *Handler) CreateClientKey(w http.ResponseWriter, r *http.Request) {
	if h.Keys == nil {
		writeNotReady(w)
		return
	}
	var req CreateClientKeyRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	created, err := h.Keys.Create(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, created)
}

// GetClientKey GET /api/admin/client-keys/{id}
func (h *Handler) GetClientKey(w http.ResponseWriter, r *http.Request) {
	if h.Keys == nil {
		writeNotReady(w)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	item, err := h.Keys.Get(id)
	if err != nil || item == nil {
		writeErr(w, http.StatusNotFound, "client key not found")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// UpdateClientKey PUT/PATCH /api/admin/client-keys/{id}
func (h *Handler) UpdateClientKey(w http.ResponseWriter, r *http.Request) {
	if h.Keys == nil {
		writeNotReady(w)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	var req UpdateClientKeyRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	item, err := h.Keys.Update(id, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if item == nil {
		writeErr(w, http.StatusNotFound, "client key not found")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// DeleteClientKey DELETE /api/admin/client-keys/{id}
func (h *Handler) DeleteClientKey(w http.ResponseWriter, r *http.Request) {
	if h.Keys == nil {
		writeNotReady(w)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if err := h.Keys.Delete(id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": id})
}
