package adminapi

import (
	"net/http"

	"prism-2api/internal/groups"
)

func (h *Handler) groupItem(g groups.Group) Group {
	item := Group{
		Name:        g.Name,
		Description: g.Description,
		CreatedAt:   g.CreatedAt,
	}
	if g.Config != nil {
		item.RPM = g.Config.RPM
		item.DailyMax = g.Config.DailyMax
	}
	if h.Pool != nil {
		for _, s := range h.Pool.List() {
			for _, name := range s.Groups {
				if name == g.Name {
					item.CredentialCount++
					break
				}
			}
		}
	}
	if h.Keys != nil {
		for _, k := range h.Keys.List() {
			if k.Group == g.Name {
				item.ClientKeyCount++
			}
		}
	}
	return item
}

// ListGroups GET /api/admin/groups
func (h *Handler) ListGroups(w http.ResponseWriter, r *http.Request) {
	if h.Groups == nil {
		writeNotReady(w)
		return
	}
	raw := h.Groups.List()
	out := make([]Group, 0, len(raw))
	for _, g := range raw {
		out = append(out, h.groupItem(g))
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out, "total": len(out)})
}

// CreateGroup POST /api/admin/groups
func (h *Handler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	if h.Groups == nil {
		writeNotReady(w)
		return
	}
	var req CreateGroupRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	g, err := h.Groups.Create(req.Name, req.Description)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	item := h.groupItem(g)
	writeJSON(w, http.StatusOK, item)
}

// GetGroup GET /api/admin/groups/{name}
func (h *Handler) GetGroup(w http.ResponseWriter, r *http.Request) {
	if h.Groups == nil {
		writeNotReady(w)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	g, ok := h.Groups.Get(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	item := h.groupItem(g)
	writeJSON(w, http.StatusOK, item)
}

// UpdateGroup PATCH /api/admin/groups/{name}
func (h *Handler) UpdateGroup(w http.ResponseWriter, r *http.Request) {
	if h.Groups == nil {
		writeNotReady(w)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	var req UpdateGroupRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if _, ok := h.Groups.Get(name); !ok {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	current := name
	if req.NewName != nil && *req.NewName != "" && *req.NewName != name {
		g, err := h.Groups.Rename(name, *req.NewName)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		current = g.Name
	}
	var cfg *groups.Config
	if req.RPM != nil || req.DailyMax != nil {
		cur, _ := h.Groups.Get(current)
		c := groups.Config{}
		if cur.Config != nil {
			c = *cur.Config
		}
		if req.RPM != nil {
			c.RPM = *req.RPM
		}
		if req.DailyMax != nil {
			c.DailyMax = *req.DailyMax
		}
		cfg = &c
	}
	if req.Description != nil || cfg != nil {
		g, err := h.Groups.Update(current, req.Description, cfg)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		item := h.groupItem(g)
		writeJSON(w, http.StatusOK, item)
		return
	}
	g, _ := h.Groups.Get(current)
	item := h.groupItem(g)
	writeJSON(w, http.StatusOK, item)
}

// DeleteGroup DELETE /api/admin/groups/{name}?force=1
func (h *Handler) DeleteGroup(w http.ResponseWriter, r *http.Request) {
	if h.Groups == nil {
		writeNotReady(w)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	item := h.groupItem(groups.Group{Name: name})
	if !force && (item.CredentialCount > 0 || item.ClientKeyCount > 0) {
		writeErr(w, http.StatusConflict, "group still referenced; pass force=1 to delete")
		return
	}
	if force && h.Keys != nil {
		empty := ""
		for _, k := range h.Keys.List() {
			if k.Group == name {
				_, _ = h.Keys.Update(k.ID, UpdateClientKeyRequest{Group: &empty})
			}
		}
	}
	if force && h.Pool != nil {
		for _, a := range h.Pool.Accounts() {
			var next []string
			for _, g := range a.Groups() {
				if g != name {
					next = append(next, g)
				}
			}
			a.SetGroups(next)
		}
	}
	if !h.Groups.Delete(name) {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": name})
}
