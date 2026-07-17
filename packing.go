package main

// Manifest — packing lists. Master lists carry the "always pack this"
// knowledge; each holiday gets its own copy to tick off, and anything
// learned mid-trip can be promoted back onto a master list.

import (
	"net/http"
	"strconv"
	"strings"
)

const maxLabelLen = 80

func cleanLabel(w http.ResponseWriter, s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		httpError(w, http.StatusBadRequest, "item cannot be empty")
		return "", false
	}
	if len(s) > maxLabelLen {
		s = s[:maxLabelLen]
	}
	return s, true
}

// --- master lists ---

type templateItemOut struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

type templateOut struct {
	ID    int64             `json:"id"`
	Name  string            `json:"name"`
	Items []templateItemOut `json:"items"`
}

func (a *app) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`
		SELECT t.id, t.name, COALESCE(i.id, 0), COALESCE(i.label, '')
		FROM packing_templates t
		LEFT JOIN packing_template_items i ON i.template_id = t.id
		ORDER BY t.name, i.sort, i.id`)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []*templateOut{}
	byID := map[int64]*templateOut{}
	for rows.Next() {
		var tid int64
		var name string
		var it templateItemOut
		if err := rows.Scan(&tid, &name, &it.ID, &it.Label); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		t := byID[tid]
		if t == nil {
			t = &templateOut{ID: tid, Name: name, Items: []templateItemOut{}}
			byID[tid] = t
			out = append(out, t)
		}
		if it.ID != 0 {
			t.Items = append(t.Items, it)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string   `json:"name"`
		Items []string `json:"items"` // optional: create pre-filled (starter lists)
	}
	if !readJSON(w, r, &req) {
		return
	}
	name, ok := cleanLabel(w, req.Name)
	if !ok {
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO packing_templates (name) VALUES (?)`, name)
	if err != nil {
		httpError(w, http.StatusConflict, "a master list with that name already exists")
		return
	}
	id, _ := res.LastInsertId()
	for i, label := range req.Items {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		if len(label) > maxLabelLen {
			label = label[:maxLabelLen]
		}
		if _, err := tx.Exec(`INSERT INTO packing_template_items (template_id, label, sort)
			VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, id, label, i); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (a *app) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a.db.Exec(`DELETE FROM packing_templates WHERE id = ?`, id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleAddTemplateItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Label string `json:"label"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	label, ok := cleanLabel(w, req.Label)
	if !ok {
		return
	}
	res, err := a.db.Exec(`INSERT INTO packing_template_items (template_id, label, sort)
		SELECT ?, ?, COALESCE(MAX(sort) + 1, 0) FROM packing_template_items WHERE template_id = ?`,
		id, label, id)
	if err != nil {
		httpError(w, http.StatusConflict, "already on this list")
		return
	}
	itemID, _ := res.LastInsertId()
	writeJSON(w, http.StatusCreated, templateItemOut{ID: itemID, Label: label})
}

func (a *app) handleDeleteTemplateItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	itemID, err := strconv.ParseInt(r.PathValue("itemID"), 10, 64)
	if err != nil {
		httpError(w, http.StatusBadRequest, "bad id")
		return
	}
	a.db.Exec(`DELETE FROM packing_template_items WHERE id = ? AND template_id = ?`, itemID, id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- a holiday's manifest ---

type packingItemOut struct {
	ID      int64  `json:"id"`
	Label   string `json:"label"`
	Checked bool   `json:"checked"`
}

func (a *app) handleListPacking(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rows, err := a.db.Query(`
		SELECT id, label, checked FROM packing_items WHERE holiday_id = ? ORDER BY sort, id`, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []packingItemOut{}
	for rows.Next() {
		var it packingItemOut
		if err := rows.Scan(&it.ID, &it.Label, &it.Checked); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleAddPackingItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Label string `json:"label"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	label, ok := cleanLabel(w, req.Label)
	if !ok {
		return
	}
	res, err := a.db.Exec(`INSERT INTO packing_items (holiday_id, label, sort)
		SELECT ?, ?, COALESCE(MAX(sort) + 1, 0) FROM packing_items WHERE holiday_id = ?`,
		id, label, id)
	if err != nil {
		httpError(w, http.StatusConflict, "already on the manifest")
		return
	}
	itemID, _ := res.LastInsertId()
	writeJSON(w, http.StatusCreated, packingItemOut{ID: itemID, Label: label})
}

// handleApplyTemplate copies a master list onto a holiday's manifest;
// items already there (same label) are left untouched.
func (a *app) handleApplyTemplate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		TemplateID int64 `json:"template_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	res, err := a.db.Exec(`
		INSERT INTO packing_items (holiday_id, label, sort)
		SELECT ?, ti.label,
		       ti.sort + (SELECT COALESCE(MAX(sort) + 1, 0) FROM packing_items WHERE holiday_id = ?)
		FROM packing_template_items ti WHERE ti.template_id = ?
		ON CONFLICT DO NOTHING`, id, id, req.TemplateID)
	if err != nil {
		httpError(w, http.StatusBadRequest, "no such holiday or master list")
		return
	}
	added, _ := res.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]int64{"added": added})
}

func (a *app) handleUpdatePackingItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Checked *bool `json:"checked"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Checked != nil {
		a.db.Exec(`UPDATE packing_items SET checked = ? WHERE id = ?`, *req.Checked, id)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleDeletePackingItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a.db.Exec(`DELETE FROM packing_items WHERE id = ?`, id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePromoteItem copies a manifest item onto a master list — the
// "always pack this" loop that makes the lists sharper every trip.
func (a *app) handlePromoteItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		TemplateID int64 `json:"template_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	res, err := a.db.Exec(`
		INSERT INTO packing_template_items (template_id, label, sort)
		SELECT ?, pi.label,
		       (SELECT COALESCE(MAX(sort) + 1, 0) FROM packing_template_items WHERE template_id = ?)
		FROM packing_items pi WHERE pi.id = ?
		ON CONFLICT DO NOTHING`, req.TemplateID, req.TemplateID, id)
	if err != nil {
		httpError(w, http.StatusBadRequest, "no such master list")
		return
	}
	added, _ := res.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]bool{"added": added > 0})
}
