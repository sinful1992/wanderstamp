FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /holidaymap .

FROM scratch
COPY --from=build /holidaymap /holidaymap
USER 65534
ENTRYPOINT ["/holidaymap"]
