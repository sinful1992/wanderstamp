package main

// Task 142 (audit 137 WS-1): the parts of a trip /api/export used to leave
// out. Without them a move to a new server lost the GPX tracks, and the
// photos set aside by hand came back onto the map (and share links and the
// TV) at the first sync. Share links, paired TVs, users and sessions are not
// exported on purpose: they are about who may look, not about the trips.

import (
	"database/sql"
	"encoding/json"
)

type trackExport struct {
	Name   string          `json:"name"`
	Points json.RawMessage `json:"points"` // [[lat,lng],...], at most maxTrackPoints
}

type asideExport struct {
	AssetID string `json:"asset_id"`
	TakenAt string `json:"taken_at"`
	SetAt   string `json:"set_at"`
}

type unplacedExport struct {
	AssetID  string  `json:"asset_id"`
	TakenAt  string  `json:"taken_at"`
	Type     string  `json:"type"`
	Duration float64 `json:"duration"`
}

// choiceExport is one "same place as this pin, or its own stop?" answer.
// PinID is the pin's id in this export (pins[].id); 0 = its own stop.
type choiceExport struct {
	AssetID string `json:"asset_id"`
	PinID   int64  `json:"pin_id"`
}

type templateExport struct {
	Name  string   `json:"name"`
	Items []string `json:"items"`
}

// holidayRest is embedded in the export's trip, so its fields sit beside the
// others in the JSON.
type holidayRest struct {
	Tracks       []trackExport    `json:"tracks"`
	SetAside     []asideExport    `json:"set_aside"`
	Unplaced     []unplacedExport `json:"unplaced"`
	PhotoChoices []choiceExport   `json:"photo_choices"`
}

func newHolidayRest() holidayRest {
	return holidayRest{Tracks: []trackExport{}, SetAside: []asideExport{}, Unplaced: []unplacedExport{}, PhotoChoices: []choiceExport{}}
}

// exportRest fills each trip's tracks, set-aside and unplaced photos and
// same-place answers, and returns the master packing lists.
func (a *app) exportRest(rest map[int64]*holidayRest) ([]templateExport, error) {
	each := func(q string, scan func(*sql.Rows) error) error {
		rows, err := a.db.Query(q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := each(`SELECT holiday_id, name, points FROM tracks ORDER BY holiday_id, id`, func(r *sql.Rows) error {
		var hid int64
		var t trackExport
		var pts string
		if err := r.Scan(&hid, &t.Name, &pts); err != nil {
			return err
		}
		t.Points = json.RawMessage(pts)
		if h := rest[hid]; h != nil && json.Valid(t.Points) {
			h.Tracks = append(h.Tracks, t)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := each(`SELECT holiday_id, asset_id, taken_at, set_at FROM set_aside ORDER BY holiday_id, taken_at, asset_id`, func(r *sql.Rows) error {
		var hid int64
		var x asideExport
		if err := r.Scan(&hid, &x.AssetID, &x.TakenAt, &x.SetAt); err != nil {
			return err
		}
		if h := rest[hid]; h != nil {
			h.SetAside = append(h.SetAside, x)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := each(`SELECT holiday_id, asset_id, taken_at, type, duration FROM unplaced_photos ORDER BY holiday_id, taken_at, asset_id`, func(r *sql.Rows) error {
		var hid int64
		var x unplacedExport
		if err := r.Scan(&hid, &x.AssetID, &x.TakenAt, &x.Type, &x.Duration); err != nil {
			return err
		}
		if h := rest[hid]; h != nil {
			h.Unplaced = append(h.Unplaced, x)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := each(`SELECT holiday_id, asset_id, pin_id FROM photo_choices ORDER BY holiday_id, asset_id`, func(r *sql.Rows) error {
		var hid int64
		var x choiceExport
		if err := r.Scan(&hid, &x.AssetID, &x.PinID); err != nil {
			return err
		}
		if h := rest[hid]; h != nil {
			h.PhotoChoices = append(h.PhotoChoices, x)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	templates := []templateExport{}
	byID := map[int64]int{}
	if err := each(`SELECT t.id, t.name, i.label FROM packing_templates t LEFT JOIN packing_template_items i ON i.template_id = t.id
		ORDER BY t.name, t.id, i.sort, i.id`, func(r *sql.Rows) error {
		var id int64
		var name string
		var label sql.NullString
		if err := r.Scan(&id, &name, &label); err != nil {
			return err
		}
		i, ok := byID[id]
		if !ok {
			templates = append(templates, templateExport{Name: name, Items: []string{}})
			i = len(templates) - 1
			byID[id] = i
		}
		if label.Valid {
			templates[i].Items = append(templates[i].Items, label.String)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return templates, nil
}
