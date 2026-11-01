package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// F2: a delete is a real delete, but the rows it removes are kept in memory
// for undoWindow, so "Undo" can put them back with the same ids. Nothing
// reads a soft-deleted row because there is none.

const undoWindow = 30 * time.Second

type snapRows struct {
	table string
	cols  []string
	rows  [][]any
}

type undoSnap struct {
	tables    []snapRows // parents before children: restore in this order
	holidayID int64      // the trip touched, for a re-sync after restore
	at        time.Time
}

type undoStore struct {
	mu    sync.Mutex
	snaps map[string]undoSnap
}

func (u *undoStore) put(s undoSnap) string {
	b := make([]byte, 16)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.snaps == nil {
		u.snaps = map[string]undoSnap{}
	}
	for k, v := range u.snaps { // a handful at most; keep the map from growing
		if time.Since(v.at) > undoWindow {
			delete(u.snaps, k)
		}
	}
	u.snaps[tok] = s
	return tok
}

// take hands out a snapshot once; a second undo of the same token is too late.
func (u *undoStore) take(tok string) (undoSnap, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.snaps[tok]
	delete(u.snaps, tok)
	if !ok || time.Since(s.at) > undoWindow {
		return undoSnap{}, false
	}
	return s, true
}

type fkChild struct{ table, from, to string }

// cascadeChildren lists, from the schema itself, every table whose rows go
// when a row of parent is deleted (ON DELETE CASCADE). Reading the schema
// rather than a fixed list means a table added later (tracks) is covered.
func cascadeChildren(q queryer, parent string) ([]fkChild, error) {
	rows, err := q.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	var out []fkChild
	for _, t := range tables {
		fks, err := q.Query(`SELECT "table", "from", "to", on_delete FROM pragma_foreign_key_list(?)`, t)
		if err != nil {
			return nil, err
		}
		for fks.Next() {
			var pt, from, onDelete string
			var to sql.NullString
			fks.Scan(&pt, &from, &to, &onDelete)
			if pt == parent && strings.EqualFold(onDelete, "CASCADE") {
				col := "id"
				if to.Valid && to.String != "" {
					col = to.String
				}
				out = append(out, fkChild{t, from, col})
			}
		}
		fks.Close()
	}
	return out, nil
}

type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

func ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// snapshot reads the row(s) of table where col = val and, recursively, every
// row that cascades from them.
func snapshot(q queryer, table, col string, val any, out *[]snapRows) error {
	rows, err := q.Query(`SELECT * FROM `+ident(table)+` WHERE `+ident(col)+` = ?`, val)
	if err != nil {
		return err
	}
	cols, _ := rows.Columns()
	sr := snapRows{table: table, cols: cols}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return err
		}
		sr.rows = append(sr.rows, vals)
	}
	rows.Close()
	if len(sr.rows) == 0 {
		return nil
	}
	*out = append(*out, sr)
	children, err := cascadeChildren(q, table)
	if err != nil {
		return err
	}
	for _, c := range children {
		ci := -1
		for i, n := range cols {
			if n == c.to {
				ci = i
			}
		}
		if ci < 0 {
			return fmt.Errorf("undo: %s has no column %s", table, c.to)
		}
		for _, r := range sr.rows {
			if err := snapshot(q, c.table, c.from, r[ci], out); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteWithUndo snapshots and deletes one row (and its cascade) in one
// transaction. ok=false: there was no such row.
func (a *app) deleteWithUndo(table string, id, holidayID int64) (string, bool, error) {
	tx, err := a.db.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var snap []snapRows
	if err := snapshot(tx, table, "id", id, &snap); err != nil {
		return "", false, err
	}
	if len(snap) == 0 {
		return "", false, nil
	}
	if _, err := tx.Exec(`DELETE FROM `+ident(table)+` WHERE id = ?`, id); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return a.undo.put(undoSnap{tables: snap, holidayID: holidayID, at: time.Now()}), true, nil
}

// errUndoConflict: the rows can't go back as they were (an id was reused, or
// a restored live trip would make two live trips).
var errUndoConflict = fmt.Errorf("undo conflict")

func (a *app) restore(s undoSnap) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, t := range s.tables {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(t.cols)), ",")
		names := make([]string, len(t.cols))
		for j, c := range t.cols {
			names[j] = ident(c)
		}
		// The deleted row itself must come back exactly; if its id was taken
		// meanwhile that's a conflict. Its children may meet a photo a sync
		// has filed elsewhere since (asset_id is UNIQUE): theirs wins back.
		verb := "INSERT"
		if i > 0 {
			verb = "INSERT OR REPLACE"
		}
		stmt := verb + ` INTO ` + ident(t.table) + ` (` + strings.Join(names, ",") + `) VALUES (` + ph + `)`
		for _, r := range t.rows {
			if _, err := tx.Exec(stmt, r...); err != nil {
				if strings.Contains(err.Error(), "constraint") {
					return errUndoConflict
				}
				return err
			}
		}
	}
	return tx.Commit()
}

func (a *app) handleUndo(w http.ResponseWriter, r *http.Request) {
	s, ok := a.undo.take(r.PathValue("token"))
	if !ok {
		httpError(w, http.StatusGone, "too late to undo")
		return
	}
	if err := a.restore(s); err == errUndoConflict {
		httpError(w, http.StatusConflict, "can't undo: another trip is live, or the item was replaced")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	if s.holidayID != 0 {
		a.resyncSoon(s.holidayID)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleReopenHoliday makes an ended trip live again (F2: "Reopen trip").
func (a *app) handleReopenHoliday(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var planned bool
	var end sql.NullString
	err := a.db.QueryRow(`SELECT planned, end_at FROM holidays WHERE id = ?`, id).Scan(&planned, &end)
	if err == sql.ErrNoRows {
		httpError(w, http.StatusNotFound, "no such holiday")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	if planned || !end.Valid {
		httpError(w, http.StatusBadRequest, "only an ended trip can be reopened")
		return
	}
	if _, err := a.db.Exec(`UPDATE holidays SET end_at = NULL WHERE id = ?`, id); err != nil {
		if strings.Contains(err.Error(), "constraint") {
			httpError(w, http.StatusConflict, "another trip is live")
			return
		}
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	a.resyncSoon(id)
	h, err := a.holidayByID(id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// holidayByID is one trip as the map lists it.
func (a *app) holidayByID(id int64) (holidayOut, error) {
	pins, err := a.queryPins(id)
	if err != nil {
		return holidayOut{}, err
	}
	hs, err := a.listHolidays(pins)
	if err != nil {
		return holidayOut{}, err
	}
	for _, h := range hs {
		if h.ID == id {
			return h, nil
		}
	}
	return holidayOut{}, sql.ErrNoRows
}
