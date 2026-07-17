FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /holidaymap .

FROM scratch
# CA roots for outbound HTTPS (Nominatim place search)
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /holidaymap /holidaymap
USER 65534
ENTRYPOINT ["/holidaymap"]
