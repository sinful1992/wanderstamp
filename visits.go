package main

import (
	"sort"
	"time"
)

// --- visits: when each place was actually visited ---
//
// A pin is a place, not a moment. A trip that goes Hinckley → Warwick →
// Hinckley has one Hinckley pin, and ordering the story by each pin's first
// photo filed every day at the hotel into one chapter and drew the route
// through it once. So each trip's photos are walked in the order they were
// taken, and every stretch spent at one pin becomes a visit: the story has a
// chapter per visit and the route line goes through them in turn.

// A visit is one stay at a pin: from the first photo there to the last, with
// how many photos it holds (0 for a pin dropped with nothing taken).
type visit struct {
	At    string `json:"at"`
	Until string `json:"until"`
	N     int    `json:"n"`
}

// shot is a photo's pin and time, all that visits are made of.
type shot struct {
	pin int64
	at  string
}

// A stay at one pin splits where the night falls: the evening at a hotel and
// the next breakfast there are two chapters, one on each day. The server
// knows no time zone, so "day" is local solar time from the pin's longitude
// (15° an hour), turning over at 4 am — a late photo still belongs to the
// evening. A whole day in one place with a long gap between photos stays one.
const dayTurns = 4 * time.Hour

func localDay(at string, lng float64) string {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return at[:min(len(at), 10)]
	}
	solar := time.Duration(lng / 15 * float64(time.Hour))
	return t.Add(solar - dayTurns).Format("2006-01-02")
}

// fillVisits sets each pin's visits and moves visited_at to the first of
// them. A pin placed by hand with no photos is visited when it was dropped —
// there is nothing else to go by. One with photos counts its drop only on the
// day of its first photo, earlier that day: arriving and pinning before the
// camera comes out. A pin tapped onto the map days ahead, as a plan, is not
// a visit that day, and one added after the fact says nothing either.
func fillVisits(pins []pinOut, shots []shot) {
	type event struct {
		pin   int
		at    string
		photo bool
	}
	idx := make(map[int64]int, len(pins))
	for i := range pins {
		idx[pins[i].ID] = i
		pins[i].Visits = []visit{}
	}
	first := make(map[int64]string)
	byTrip := make(map[int64][]event)
	for _, s := range shots {
		i, ok := idx[s.pin]
		if !ok {
			continue
		}
		if f, seen := first[s.pin]; !seen || s.at < f {
			first[s.pin] = s.at
		}
		byTrip[pins[i].HolidayID] = append(byTrip[pins[i].HolidayID], event{i, s.at, true})
	}
	for i, p := range pins {
		f, has := first[p.ID]
		if p.Kind == "manual" && (!has || p.CreatedAt < f && localDay(p.CreatedAt, p.Lng) == localDay(f, p.Lng)) {
			byTrip[p.HolidayID] = append(byTrip[p.HolidayID], event{i, p.CreatedAt, false})
		}
	}
	for _, evs := range byTrip {
		sort.Slice(evs, func(a, b int) bool {
			if evs[a].at != evs[b].at {
				return evs[a].at < evs[b].at
			}
			return pins[evs[a].pin].ID < pins[evs[b].pin].ID
		})
		last := -1 // pin of the visit still open
		for _, e := range evs {
			vs := pins[e.pin].Visits
			lng := pins[e.pin].Lng
			if e.pin == last && len(vs) > 0 && localDay(vs[len(vs)-1].Until, lng) == localDay(e.at, lng) {
				v := &vs[len(vs)-1]
				v.Until = e.at
				if e.photo {
					v.N++
				}
			} else {
				v := visit{At: e.at, Until: e.at}
				if e.photo {
					v.N = 1
				}
				pins[e.pin].Visits = append(vs, v)
			}
			last = e.pin
		}
	}
	for i := range pins {
		if len(pins[i].Visits) > 0 {
			pins[i].VisitedAt = pins[i].Visits[0].At
		}
	}
}
