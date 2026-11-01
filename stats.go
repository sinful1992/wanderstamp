package main

import (
	"math"
	"net/http"
	"sort"
	"time"
)

// F4: the passport's numbers. Planned trips haven't happened, so they don't count.

type statYear struct {
	Year  int     `json:"year"`
	Trips int     `json:"trips"`
	Days  int     `json:"days"`
	KM    float64 `json:"km"`
}

type statTrip struct {
	ID   int64   `json:"id"`
	Name string  `json:"name"`
	Year int     `json:"year"`
	KM   float64 `json:"km"`
}

type statCountry struct {
	Country     string `json:"country"`
	Visits      int    `json:"visits"` // trips with a pin in it
	FirstTripID int64  `json:"first_trip_id"`
	FirstAt     string `json:"first_at"`
}

// calDays counts the calendar days (UTC) a trip touches, first and last
// included; a live trip runs to today.
func calDays(start string, end *string, now time.Time) int {
	s, err := time.Parse(time.RFC3339, start)
	if err != nil {
		return 0
	}
	e := now
	if end != nil {
		if t, err := time.Parse(time.RFC3339, *end); err == nil {
			e = t
		}
	}
	s, e = s.UTC(), e.UTC()
	sd := time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, time.UTC)
	ed := time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, time.UTC)
	if ed.Before(sd) {
		return 1
	}
	return int(ed.Sub(sd).Hours()/24) + 1
}

// tripKM sums the great-circle legs between a trip's stops in visit order.
// A stop is a visit, not a pin: London → Paris → London again is two legs,
// as the map's route and the story draw it.
func tripKM(pins []pinOut) float64 {
	type stop struct {
		at       string
		id       int64
		lat, lng float64
	}
	var stops []stop
	for _, p := range pins {
		if len(p.Visits) == 0 {
			stops = append(stops, stop{p.VisitedAt, p.ID, p.Lat, p.Lng})
			continue
		}
		for _, v := range p.Visits {
			stops = append(stops, stop{v.At, p.ID, p.Lat, p.Lng})
		}
	}
	sort.SliceStable(stops, func(i, j int) bool { return stops[i].at < stops[j].at })
	km := 0.0
	for i := 1; i < len(stops); i++ {
		if stops[i].id != stops[i-1].id {
			km += haversineKM(stops[i-1].lat, stops[i-1].lng, stops[i].lat, stops[i].lng)
		}
	}
	return math.Round(km*10) / 10
}

func (a *app) handleStats(w http.ResponseWriter, r *http.Request) {
	pins, err := a.queryPins(0)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	byTrip := map[int64][]pinOut{}
	for _, p := range pins {
		byTrip[p.HolidayID] = append(byTrip[p.HolidayID], p)
	}
	countryRows, err := a.reader().Query(`SELECT DISTINCT holiday_id, country FROM pins WHERE country != ''`)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	tripCountries := map[int64][]string{}
	for countryRows.Next() {
		var id int64
		var c string
		countryRows.Scan(&id, &c)
		tripCountries[id] = append(tripCountries[id], c)
	}
	countryRows.Close()

	rows, err := a.reader().Query(`SELECT id, name, start_at, end_at FROM holidays WHERE planned = 0 ORDER BY start_at, id`)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	now := time.Now().UTC()
	years := map[int]*statYear{}
	countries := map[string]*statCountry{}
	out := struct {
		Years     []statYear    `json:"years"`
		Trips     []statTrip    `json:"trips"`
		Countries []statCountry `json:"countries"`
	}{[]statYear{}, []statTrip{}, []statCountry{}}
	for rows.Next() {
		var id int64
		var name, start string
		var end *string
		if err := rows.Scan(&id, &name, &start, &end); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		st, err := time.Parse(time.RFC3339, start)
		if err != nil {
			continue
		}
		year := st.UTC().Year()
		km := tripKM(byTrip[id])
		out.Trips = append(out.Trips, statTrip{id, name, year, km})
		y := years[year]
		if y == nil {
			y = &statYear{Year: year}
			years[year] = y
		}
		y.Trips++
		y.Days += calDays(start, end, now)
		y.KM = math.Round((y.KM+km)*10) / 10
		for _, c := range tripCountries[id] {
			if sc := countries[c]; sc != nil {
				sc.Visits++
			} else { // trips come oldest first, so the first seen is the first visit
				countries[c] = &statCountry{c, 1, id, start}
			}
		}
	}
	for _, y := range years {
		out.Years = append(out.Years, *y)
	}
	sort.Slice(out.Years, func(i, j int) bool { return out.Years[i].Year > out.Years[j].Year })
	for _, c := range countries {
		out.Countries = append(out.Countries, *c)
	}
	sort.Slice(out.Countries, func(i, j int) bool {
		if out.Countries[i].Visits != out.Countries[j].Visits {
			return out.Countries[i].Visits > out.Countries[j].Visits
		}
		return out.Countries[i].Country < out.Countries[j].Country
	})
	writeJSON(w, http.StatusOK, out)
}
