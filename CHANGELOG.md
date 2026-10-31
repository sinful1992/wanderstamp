# Changelog

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
