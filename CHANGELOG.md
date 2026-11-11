# Changelog

## 1.20.0 (2026-11-11)

### Frontend (ada, task 142)
- A pin deleted on another phone drops off the map as soon as you try to save it; what you typed stays (a save answered 404 reloads the map; before, it said "Pin saved" over nothing, and since WS-3 it showed the error and kept the pin).
- Test only: the visit-notes browser test no longer fails now and then when a map reload is still in flight as the test closes its page.

### Server (task 142, audit 137 WS-1..4)
- The ids hand-placed pins carry (1.18.0, so a pin sent twice is stored once) are forgotten after 90 days, checked every hour; before, one row per pin stayed forever. A phone that sends a 90-day-old queued pin again stores it as a new pin, as a page from before 1.18.0 did.
- Export and import now carry everything a move to a new server needs (WS-1). Before, the export left out GPX tracks, photos set aside by hand, photos with no position, the "same place?" answers, the master packing lists, who started a trip, added a pin or wrote the journal, and the trip's Immich album; after export + import the set-aside photos came back onto the map, the share links and the TV at the next sync, and the tracks were gone.
  - Per trip: `tracks`, `set_aside`, `unplaced`, `photo_choices` (pointing at the export's own pin ids, mapped to the new ids on import; an answer whose pin is gone is dropped and asked again), `created_by`, `journal_by`, `immich_album_id`; per pin `created_by` and, per photo, an open "same place?" question (`ask`). At the top: `packing_templates`, each with its items. A list whose name is already here (any case) gets only the items it lacks.
  - Every new field is optional: an export from before 1.20.0 imports as before, with the same answer. A bad new field (a track point off the map, a bad asset id or time) refuses the whole file and changes nothing. A photo is filed in one place only: one already on a pin, unplaced or set aside here stays there.
  - Not exported, on purpose: share links (bringing back an old trip must not bring back a link that was turned off), paired TVs, users and sessions.
- Editing a trip or a pin checks everything first and writes it all in one go: a request refused with 400 (an empty name, a bad colour, a cover photo from another trip) no longer leaves the dates or journal half-changed, a trip or pin that does not exist answers 404 instead of `{"ok":true}`, and a failed write answers 500 instead of passing silently (WS-3). A visit note and the pin's other fields land together.
- Moving a pin to a position no map has (lat outside -90..90, lng outside -180..180, or only one of the two) is refused like on create. Before, one such pin made the import refuse the whole export file (WS-2).
- The TV door lets in a TV that reaches the server over IPv6 link-local (`fe80::1%eth0`): Go gives such an address with its zone, which the door could not read, so the TV got 404. A zone is accepted only on a link-local address (WS-4).

## 1.19.0 (2026-11-11)

### Frontend (ada, task 142)
- Signing out never hands one person's waiting pins to the next: pins queued offline keep their owner, sign-out sends them first, and a sign-out of an already expired session (the server answers 401) still counts as signed out and forgets the saved map. A map answer that arrives during sign-out is no longer saved again (audit 137 WA-3).
- Opening the app with an expired session forgets the saved map and clears it from the screen; opening it offline keeps the map as before (WA-4).
- A pin placed on a repeated copy of the world (scrolled past the date line) is saved with its longitude wrapped to -180..180, so it shows in the right place on every device (WS-7).

### CI (task 142, WS-8 follow-ups from ada's review)
- Only one image publish runs at a time (`concurrency: publish-image` on the publish job), so two tags pushed together no longer race for `latest`.
- The workflow check (`.github/tests/workflow-gate.test.mjs`) is stricter: comments are ignored; the publish job's `if:` must be exactly the tag check (no `always()`, `!cancelled()`, `failure() ||`, `|| workflow_dispatch`, negated or `contains`); any `docker/build-push-action`, `buildx --push`, `type=registry` or `push: ${{ }}` counts as publishing; `test.yml` may not swallow a failure (`|| true`, `continue-on-error` by expression) or skip a check with an `if:`; the test job must call exactly `test.yml`. 16 of the 18 holes a review found in the task 141 check are closed (12 tests); the other two are repo settings, not workflow text.

### Server
- Fixed: when Immich answered a photo sync with only part of a trip's photos (a library offline, a search index being rebuilt), the sync removed the missing photos and deleted every emptied photo pin, losing its typed title and cover. Now each photo the search left out is checked with Immich on its own, and it is removed only if Immich says it is gone: not found, in the trash, archived or hidden, or now dated outside the trip. An offline photo (its library cannot be reached, so Immich trashes it) is never removed. If more than half of a trip's photos come back gone at once (a key for another account says "not found" for all of them), or more than 100 are missing from the search, that sync removes nothing but still adds new photos; if a check fails or takes too long (10 s each, 60 s in all), the sync stops and changes nothing (audit 137 WS-6, with ada's review).
- A normal sync, where the search returns every photo, makes no extra calls.

### CI
- A version tag now publishes the server image only after a new test job passes: `go vet`, `go test`, the browser tests (`webtest/run.mjs`, headless Chromium) and a check of the workflow files themselves (`.github/workflows/test.yml`, called by `docker.yml`; audit 137 WS-8). The same job runs on every push and pull request.
- A manual run of "Publish Docker image" on a branch runs the tests and skips the publish; publishing (including `latest`) happens only from a `v*` tag. Before, a manual run on master pushed `latest`.
- `.github/tests/workflow-gate.test.mjs` (5 tests, `node --test .github/tests/*.test.mjs`) fails on the old `docker.yml` (no test job, publish from any ref) and passes now.

## 1.18.0 (2026-11-10)

### Frontend
- Fixed: a pin saved with weak signal could appear twice when the server's answer was lost; the app now sends one id per pin and the server stores it once (WA-2/WS-5).
- Fixed: in the note editor, Cancel on "Show it instead of what you typed?" went back to the first day and Save could overwrite that day's note; it now goes back to the day you had picked (WA-1).

### Server
- `POST /api/pins` takes an optional `client_id` (1 to 64 characters of `A-Z a-z 0-9 _ : . -`), the id the page gave the pin before its first post. The same id sent again by the same user stores nothing: the answer is 201 with the pin as it is now (the repeat's own body is ignored), or 410 `pin was deleted` when that pin is gone, so a queued copy can't bring a deleted pin back. Another shape is refused with 400 `invalid client_id`. Without the field nothing changes, so pages from before 1.18.0 keep working. Keys are per user. New table `pin_keys` and new column `pins.client_id`, both added at start; no existing row is changed, and 1.17.0 still runs on the database (roll back = the old image). Keys are not part of the export: they only matter for the minutes a phone retries.

### Tests
- The audit's repro (`TestAudit137_WS5`, the same pin sent twice) now passes with a key; 13 new server tests, 10 of them fail on 1.17.0: three sends store one pin, no key stores every post, good and bad id shapes, a repeat returns the edited pin, 410 after a delete and the pin again after Undo, a repeat never returns a newer pin that got the deleted pin's number, one key used by two users, 20 posts at once, a failed post leaves no key, a deleted trip, a 1.17.0 database gets the table, and every database fault answers 500 with nothing stored. `handleCreatePin` 95.8 % of statements (the commit-failure answer is not forced), `keyedPin` 100 %.

## 1.17.0 (2026-11-09)

### Frontend
- Notes per visit: a note belongs to one visit (a place on one day). The phone's note editor asks which visit, and the TV journey shows the note only at that visit. Moving a note to another day says so before Save; typing new words on an empty day leaves the old note where it is.

### Server
- Notes per visit: new table `visit_notes` (pin + local day). Every visit in `/api/map`, the trips and the TV carries its `day` and `note`; a note whose day has no visit any more stays in the pin's `other_notes`. `PATCH /api/pins/{id}` takes `note_day` (and `from_day` to move a note) in one transaction. A pin's old note keeps showing on its first visit until it is written once. Export and import carry the notes; older exports still import. A photo stop with a visit note is never removed as empty (last photo set aside, photos gone from Immich, undo), so its note isn't lost. Share links and the TV don't receive `other_notes` (notes shown at no stop). A write that would hide or erase a note is refused with 409 and changes nothing: a plain `note` on a pin with visit notes (a page from before 1.17.0), a visit note on a pin whose old note has no visit, and a move onto a day holding other words (including an old note still on the first visit).

### Tests
- Old note on the first visit only and reads change nothing; moving a note (the Premier Inn case); two visits on one day; orphan days; bad days and missing fields; 12 writes at once; TV and share links; undo of a pin delete brings its notes back; export/import round trip. A copy of the live database boots unchanged. A stop whose only note is a visit note survives set-aside, sync and undo, and a noted twin blocks an undo. 12 break tests (ada's subagent and mine) fail on the first 1.17.0 build and pass now. Every database-error branch of the notes code is forced (SQLite triggers, a dropped table, a NULL note) and answers 500 with nothing changed; 146 of 147 new server statements are covered, and every changed line and branch of the notes screens runs in webtest.

## 1.16.0 (2026-11-09)

### Frontend
- TV pairing: the TV shows a QR code beside its 4-letter code. Scan it, tap Confirm, and the TV is paired, in the phone's browser or the Wanderstamp app (app 1.2.0). The code still works.

### Server
- TV pairing by QR code: with the new `PUBLIC_URL` setting (the address phones open the app at), `POST /tv/pair` also returns `pair_url` (`<PUBLIC_URL>/#pair=CODE`) and `qr_svg`, a QR code of it. The QR carries only the code the screen already shows, never the poll token, and pairing still needs a signed-in Confirm on the phone. Without `PUBLIC_URL` nothing changes. A `PUBLIC_URL` that isn't a plain http(s) address is ignored with a warning.

### Tests
- The QR code is read back with an independent decoder (gozxing) for every new code and for a long address; the response is unchanged when `PUBLIC_URL` is unset; the typed code still pairs. Break tests from the review: a `PUBLIC_URL` with no host name (`https://:8448`) is now refused, every URL length 11–400 and the largest that fits (version 40) decode, a too-long address leaves the plain code screen, and a 429 carries no QR.

## 1.15.4 (2026-11-08)

### Frontend
- TV journey: the map shows the whole trip and stays still; only the red dot moves from stop to stop, so it no longer blurs while moving. The photo is bigger, and a dark panel behind the title replaces the shade over the map. The dots on the map carry their stop number, the first stop is gold, and the current stop pulses.

### Tests
- TV journey: a check that the current and the next stop sit in the open map area (not off the map, nor under the postcard, the route bar, the remote hints or the title panel) at every stop of a Warwick-shaped trip and a 20-stop trip. It fails on 1.15.3, which never zooms out past 12.

## 1.15.3 (2026-11-08)

### Frontend
- TV journey: the numbered bar at the bottom is labelled Stops, so it doesn't read as days. The day stamp has a solid background, so it stays readable on any photo. Short hops slide the map at a whole zoom instead of zooming out and back, so it stays sharp; the trips screen map also rests on a whole zoom. The bar shows up to 10 stops at once, so it stays clear of the remote hints.

## 1.15.2 (2026-11-08)

### Frontend
- The `/tv` page fits the screen in a TV's WebView: it was cropped on the Shield, whose `innerWidth` grows past the screen width to fit the unscaled page.

## 1.15.1 (2026-11-08)

### Server
- The TV door also refuses its own container gateway(s), read from `/proc/net/route` at start. In Docker bridge mode a LAN device keeps its address, but traffic the host masquerades on the way in arrives from the bridge gateway, a private address. On 1.15.0 that let tailnet devices in through a Tailscale subnet router (`NoSNAT` off, the default). Now they get 404, and the start log names the refused gateway.

### TV app
- The TV app's Gradle script compiles: `java.net.URI` resolved to Gradle's `java` extension, so the APK could not be built.

## 1.15.0 (2026-11-08)

### Server
- Wanderstamp on the TV: a second, read-only listener for the home network (`TV_LISTEN_ADDR`, off unless set). It serves only the `/tv` page, its shared files, pairing and the TV reads: no sign-in, no admin, no other `/api/`, never an original photo or a video. It answers only devices on the home network (private IPv4, IPv6 link-local and unique-local); Tailscale and public addresses get 404, so it can be published on all interfaces and still start when the LAN address comes late after a power cut.
- Pairing: the TV shows a 4-letter code (`POST /tv/pair`), someone signed in types it (`POST /api/tv/pair` with a name), and the TV's waiting request (`GET /tv/pair/{poll}`) gets its own device token once, as a cookie. A TV token is not a share link: pairing never changes a trip's link. Paired TVs are listed (`GET /api/tv/devices`) and unpaired (`DELETE /api/tv/devices/{id}`); an unpaired TV is back on the code screen at its next request.
- A paired TV reads every trip that isn't planned, in the share shape (no way-there photo pins, no names): `GET /api/tv/trips`, `GET /api/tv/trips/{id}` with all the trip's photos, and `GET /api/tv/photo/{id}/thumb|preview`.
- Limits: a code lives 5 minutes; one device holds at most 3 open codes (asking again replaces its oldest one not yet typed in, so a relaunched TV never waits) and may ask for 10 a minute, 20 open codes in all; 10 wrong codes in 15 minutes stop an account typing more. A code typed in just before it runs out can still be collected for a minute. A TV whose token was never collected is removed after 15 minutes. Paths that are not clean (`..`, `//`) get 404 on the door, not a redirect. Poll ids never reach the log.

### Frontend
- The `/tv` page, made for a TV seen from the sofa and driven by the remote: a pairing screen with the 4-letter code; **Our trips**, every trip as a photo stamp with its route on the map above; **Play journey**, where the map flies from stop to stop and each photo appears as a postcard with its day stamp (6 seconds each, OK pauses, ◀ ▶ photo, ▲ ▼ stop, Back to the trips); and **All photos** in a grid with a full-screen viewer. An unpaired TV is back on the code screen within a minute; without the server it says so and tries again.
- **Show on TV** in the account page: type the code the TV shows and a name for it, see the paired TVs, and unpair one.

### TV app
- `tv-android/`: a small Android TV app (Shield, Bravia) that opens the `/tv` page full screen, keeps the screen on, keeps the pairing, and sends Back to the page first.

## 1.14.0 (2026-11-01)

### Server
- Undo: deleting a pin or a trip keeps it for 30 seconds, so Undo puts back everything that went with it (photos, packing, share link, tracks). An ended trip can be reopened.
- `GET /api/stats`: trips, days and km per year, km per trip, and every country with its first visit.
- Pins and trips remember who added them, and a journal who last wrote it (shares leave the names out).
- Videos from Immich come in with the photos. Photo lists say `type` (image or video) and `duration`, and `/api/photo/{id}/video` streams the playable copy, with seeking. A share link shows only the poster: the video file can be the original, with its GPS.
- Import (admin): `POST /api/import` loads an export back in, at most 20 MB (a leading byte-order mark is fine). It shows a dry run first, and skips trips that are already there. Exports now include each photo's type and duration.
- GPX tracks: upload a route to a trip (a .gpx file, at most 10 MB, drawn with up to 2,000 points), list it or delete it. Tracks never show on a share link.
- Undo never overwrites or moves anything done since the delete: a photo filed since on another pin, set aside, or on another trip stays where it is, and an undo that would bring back nothing says "can't undo". Undoing a photo pin still works after the photo sync has run, unless someone has changed the new pin since.
- Stats count a place you went back to as another leg (London, Paris, London again is two legs).

### Feature modules
- Routes: a trip's story draws its GPX tracks, and a Routes button adds or deletes them.
- Travel stats on the passport: totals, a year-by-year view, the furthest trips, and countries with their first visit.
- Import a backup file on Family: choose an export, see what it would add, then confirm.
- Printing a trip gives you a page with the map, then the story's stops, photos and journal. Printing the passport or the stats gives you just that page.

### Frontend
- Find a trip: once there are 5 or more, the trips list has a search box (name, place or journal; accents don't matter) and year chips. Trips you hide stay hidden after a reload, for each person on the device.
- Deleted a pin or a trip by mistake? The toast has Undo for 10 seconds. An ended trip has Reopen trip in its edit form, to make it live again.
- An open trip edit form stays open, with the fresh data, when a photo sync reloads the map; text typed while a save runs is kept.
- Day numbers on the live banner and in the story count from the day the trip started where you are (they were a day off in New York or Bangkok). A queued offline pin you discard while others are sending is not sent.
- F6: the story says who started the trip, who added each pin and who wrote the journal (nothing for old entries or on a share).
- The map loads the GPX track, stats, import and print modules: the story draws a trip's tracks, the passport has Travel stats, Family has Import a backup file.
- F3: a trip's story has All photos: the whole roll, pinned and unplaced, under its trip days.
- F7: videos show as their poster with a play mark and length, and play in the photo viewer. A share link shows the poster only, as a video can carry its GPS.
- F5: legs over 300 km are drawn as the great circle a plane flies, with a small plane pointing the way; lines no longer jump across the 180th meridian.
- D1: hand-placed pins are ringed map-maker's dots centred on the place; an open story numbers its stops on the map and in its chapters.
- P2: below region zoom a trip's crowded pins fold into one dot with a count (tap to zoom in), and photo prints fetch no thumbnail until zoomed in; 1,800 pins draw as about 50 markers.
- An undo the server refuses, because something there changed since or another trip is live, says so in plain words and reloads the map.

### Fixes from the task 112 review (planned as 1.13.1, released in 1.14.0)

#### Server
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

#### Frontend
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
