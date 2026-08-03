# syntax=docker/dockerfile:1

# Build stage. go.mod requires Go 1.26; modernc.org/sqlite is pure Go, so the
# binary is built with CGO disabled and runs on distroless.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/rables-server ./cmd/server \
    && mkdir -p /out/data

# Runtime stage. distroless/base-debian12 ships glibc, ca-certificates and
# tzdata but no shell/package manager:
#   - ca-certificates: outbound HTTPS for Mastodon/Bluesky/Twitter crossposting
#   - tzdata: settings.time_zone is resolved via time.LoadLocation
# The :nonroot variant runs as uid/gid 65532 by default.
FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /out/rables-server /usr/local/bin/rables-server
COPY --from=build --chown=nonroot:nonroot /out/data /data

ENV ADDR=:8080 \
    DATA_DIR=/data
# HMAC_SECRET is required and injected at runtime (-e HMAC_SECRET=...).

EXPOSE 8080
VOLUME ["/data"]
USER nonroot
ENTRYPOINT ["/usr/local/bin/rables-server"]
