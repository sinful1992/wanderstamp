FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# the service worker's cache name follows the frontend's content (P5), so a
# changed file can never be served from an old shell cache
RUN h=$(find static -type f ! -name sw.js | sort | xargs sha1sum | sha1sum | cut -c1-8) && sed -i "s/__BUILD__/$h/" static/sw.js && grep -q "shell-$h" static/sw.js
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /holidaymap .

FROM scratch
# CA roots for outbound HTTPS (Nominatim place search)
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /holidaymap /holidaymap
USER 65534
ENTRYPOINT ["/holidaymap"]
