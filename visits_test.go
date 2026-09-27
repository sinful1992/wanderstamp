package main

import (
	"fmt"
	"strings"
	"testing"
)

// The Warwick trip as it happened: home, the castle, the Hinckley hotel, a
// night, back to the castle, the hotel again, then Legoland and a hotel in
// Slough. One pin per place — the story needs nine stops out of five pins.
func TestVisitsFollowTheDayNotThePin(t *testing.T) {
	pins := []pinOut{
		{ID: 4, HolidayID: 2, Kind: "manual", Lng: -1.58, Title: "Warwick Castle", CreatedAt: "2026-09-25T13:30:37Z"},
		{ID: 5, HolidayID: 2, Kind: "manual", Lng: -1.40, Title: "Premier Inn", CreatedAt: "2026-09-25T17:12:49Z"},
		{ID: 6, HolidayID: 2, Kind: "photo", Title: "Totton", CreatedAt: "2026-09-26T09:44:06Z"},
		{ID: 7, HolidayID: 2, Kind: "photo", Lng: -0.65, Title: "Legoland", CreatedAt: "2026-09-27T17:24:48Z"},
		// dropped a minute after its first photo: added after the fact, no visit of its own
		{ID: 8, HolidayID: 2, Kind: "manual", Title: "Premier Inn Slough", CreatedAt: "2026-09-27T17:27:23Z"},
		// tapped onto the map on day 1 as a plan, visited on day 3: no day-1 stop
		{ID: 10, HolidayID: 2, Kind: "manual", Lng: -0.6, Title: "Windsor", CreatedAt: "2026-09-25T20:00:00Z"},
		// dropped before any photo that day: the visit starts at the drop
		{ID: 11, HolidayID: 2, Kind: "manual", Lng: -0.6, Title: "Car park", CreatedAt: "2026-09-27T16:00:00Z"},
		// another trip's pin must not interleave with this one
		{ID: 9, HolidayID: 1, Kind: "manual", Title: "Bransgore", CreatedAt: "2026-09-26T12:00:00Z"},
	}
	shots := []shot{
		{6, "2026-09-25T09:30:23Z"}, {6, "2026-09-25T09:31:00Z"},
		{4, "2026-09-25T13:21:23Z"}, {4, "2026-09-25T13:31:57Z"},
		{5, "2026-09-25T16:21:52Z"}, {5, "2026-09-25T18:08:16Z"},
		{5, "2026-09-26T08:07:35Z"}, {5, "2026-09-26T08:07:44Z"},
		{4, "2026-09-26T10:31:20Z"}, {4, "2026-09-26T11:38:20Z"},
		{5, "2026-09-26T17:15:21Z"}, {5, "2026-09-26T18:30:44Z"}, {5, "2026-09-27T02:10:00Z"}, // a 3 am photo is still that night
		{5, "2026-09-27T06:57:43Z"},
		{7, "2026-09-27T09:23:42Z"}, {7, "2026-09-27T15:24:33Z"}, // six hours apart, one day out
		{11, "2026-09-27T16:30:00Z"},
		{8, "2026-09-27T17:26:39Z"}, {8, "2026-09-27T17:42:19Z"},
		{10, "2026-09-27T19:00:00Z"},
	}
	fillVisits(pins, shots)

	type stop struct {
		title string
		v     visit
	}
	var trip []stop
	for _, p := range pins {
		if p.HolidayID == 2 {
			for _, v := range p.Visits {
				trip = append(trip, stop{p.Title, v})
			}
		}
	}
	for i := 1; i < len(trip); i++ {
		for j := i; j > 0 && trip[j].v.At < trip[j-1].v.At; j-- {
			trip[j], trip[j-1] = trip[j-1], trip[j]
		}
	}
	var got []string
	for _, s := range trip {
		got = append(got, fmt.Sprintf("%s %s×%d", s.title, s.v.At[5:10], s.v.N))
	}
	want := []string{
		"Totton 09-25×2",
		"Warwick Castle 09-25×2",
		"Premier Inn 09-25×2",
		"Premier Inn 09-26×2", // the night splits the stay
		"Warwick Castle 09-26×2",
		"Premier Inn 09-26×3",
		"Premier Inn 09-27×1", // breakfast, the next morning
		"Legoland 09-27×2",
		"Car park 09-27×1",
		"Premier Inn Slough 09-27×2",
		"Windsor 09-27×1",
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("stops:\n got  %s\n want %s", strings.Join(got, " | "), strings.Join(want, " | "))
	}
	// the evening back at the hotel is one stop, from arrival to the small hours
	if v := pins[1].Visits[2]; v.At != "2026-09-26T17:15:21Z" || v.Until != "2026-09-27T02:10:00Z" {
		t.Errorf("hotel, second night = %+v", v)
	}
	if pins[0].VisitedAt != "2026-09-25T13:21:23Z" {
		t.Errorf("castle visited_at = %s, want its first photo", pins[0].VisitedAt)
	}
	if v := pins[6].Visits[0]; v.At != "2026-09-27T16:00:00Z" {
		t.Errorf("car park visit starts %s, want the drop", v.At)
	}
	if len(pins[7].Visits) != 1 || pins[7].Visits[0].N != 0 {
		t.Errorf("the other trip's empty pin = %+v", pins[7].Visits)
	}
}
