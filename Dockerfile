# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS build

WORKDIR /src
RUN apk add --no-cache build-base
COPY go.mod ./
RUN go mod download

COPY . ./
# go.sum is generated inside the image when the host does not have Go installed.
# Tidy before building so all direct and transitive module checksums are available.
RUN go mod tidy -v
RUN CGO_ENABLED=0 GOOS=linux go build -v -trimpath -ldflags="-s -w" -o /out/playlist-server ./cmd/server

FROM alpine:3.20

RUN addgroup -S playlist && adduser -S playlist -G playlist
COPY --from=build /out/playlist-server /usr/local/bin/playlist-server
USER playlist
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/playlist-server"]
