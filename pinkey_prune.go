package main

import (
	"log"
	"time"
)

// pinKeyKeep is how long a hand-placed pin's client_id is remembered (task 142, left note of 1.18.0: the rows
// were never removed). The key only matters while a phone may still send the same queued pin again; a phone
// that comes back after more than this stores it as a new pin, as a page from before 1.18.0 did.
const pinKeyKeep = 90 * 24 * time.Hour

// prunePinKeys drops pin_keys rows older than pinKeyKeep. Runs at start and every hour (housekeeping).
func (a *app) prunePinKeys(now time.Time) int64 {
	res, err := a.db.Exec(`DELETE FROM pin_keys WHERE created_at < ?`, now.UTC().Add(-pinKeyKeep).Format(time.RFC3339))
	if err != nil {
		log.Printf("prune pin keys: %v", err)
		return 0
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("pin keys: %d older than %d days removed", n, int(pinKeyKeep.Hours()/24))
	}
	return n
}
