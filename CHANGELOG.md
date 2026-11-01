# Changelog

## 1.14.0 (unreleased)

### Frontend
- Find a trip: once there are 5 or more, the trips list has a search box (name, place or journal; accents don't matter) and year chips. Trips you hide stay hidden after a reload, for each person on the device.
- Deleted a pin or a trip by mistake? The toast has Undo for 10 seconds. An ended trip has Reopen trip in its edit form, to make it live again.
- An open trip edit form stays open, with the fresh data, when a photo sync reloads the map; text typed while a save runs is kept.
- Day numbers on the live banner and in the story count from the day the trip started where you are (they were a day off in New York or Bangkok). A queued offline pin you discard while others are sending is not sent.
F6: the story says who started the trip, who added each pin and who wrote the journal (nothing for old entries or on a share).
The map loads the GPX track, stats, import and print modules: the story draws a trip's tracks, the passport has Travel stats, Family has Import a backup file.
F3: a trip's story has All photos: the whole roll, pinned and unplaced, under its trip days.
F7: videos show as their poster with a play mark and length, and play in the photo viewer (from the share route on a share link).

## 1.13.1 (unreleased)

### Server
- Go 1.26.10, golang.org/x/crypto 0.58, modernc SQLite 1.61 (SQLite 3.54): closes the known Go standard-library
  vulnerabilities in 1.13.0's Go 1.23 build.
- Signing in ignores capitals and stray spaces in the username (an iPhone capitalises the first letter).
- Failed sign-ins are also limited per device: 20 in 15 minutes across all names.
- Requests that change something are refused when the browser says they came from another site.
- Share links can expire after 7, 30, 90 or 365 days, or never. An expired link says so.
- Share links no longer include the photo pins on the way there (they start at home), and never serve original photo
  files, which carry the camera's GPS.
- The map's data goes out gzipped (about 8x smaller), opening the map no longer writes to the database, and photos
  keep loading during a photo sync.
- One log line per server error, slow request and failed Immich call. A `-healthcheck` flag for Docker.
- After a server restart, the first Immich call retries once if Immich's name doesn't resolve yet.

### Frontend
- Trip dates show the days you picked in every timezone: in summer time a trip that ended 24 May no longer reads
  "23–25 May", and the passport's days away is no longer one too many.
- A pin dropped offline waits through server restarts and proxy errors (5xx, 429) instead of being thrown away. One pin
  the server keeps refusing with a 5xx no longer holds up the pins queued after it.
- The username box no longer capitalises on an iPhone.
- A background photo sync no longer wipes a trip or pin form you're typing in, also after a save that failed.
- The live banner's day number turns over at 4 am, the same as the trip story's.
- One tap, one request: Start, Add pin, Save, Share, Delete and Sync are off while they run, so a double tap
  can't make two trips. End holiday shows "Ending…" while it pulls the last photos.
- A failed delete, revoke, End holiday or family action says what went wrong instead of nothing.
- "1 pin", not "1 pins"; zero counts are left out; the trips pill reads "3 trips".
- Share links: pick how long a link works (30 days by default), see when it ends, and confirm before a new link
  replaces the old one. A shared page opens photo previews, never originals.
- Buttons in sentence case, so the stamps stand out. 44 px touch targets for the trip icons, colour swatches and
  zoom buttons; pins and swatches are labelled for screen readers.
- The photo viewer fits under the iPhone's address bar.
- The offline cache is named after the build, so an updated Leaflet, font or icon is never served stale.
