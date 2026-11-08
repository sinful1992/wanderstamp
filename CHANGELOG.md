# Changelog

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
