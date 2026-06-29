FROM golang:1.25-alpine AS builder

RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# -tags sqlite_fts5: enable FTS5 in mattn/go-sqlite3 (the bundled sqlite
# build does NOT include FTS5 by default; required for memory_messages_fts).
RUN CGO_ENABLED=1 go build -tags sqlite_fts5 -o /arc-relay ./cmd/arc-relay

FROM alpine:3.21

# su-exec drops to the unprivileged user from the entrypoint without
# spawning a shell process between PID 1 and the server binary.
RUN apk add --no-cache ca-certificates sqlite-libs git su-exec \
    && addgroup -g 65532 -S arcrelay \
    && adduser  -u 65532 -S -G arcrelay -H -h /nonexistent arcrelay

COPY --from=builder /arc-relay /usr/local/bin/arc-relay
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# /data is created up front with the right ownership so first-boot of a
# fresh anonymous volume requires no chown. When a pre-existing root-owned
# volume is mounted at /data, the entrypoint chowns it on startup.
RUN mkdir -p /data && chown arcrelay:arcrelay /data

# Default DB path inside the container - matches the /data volume mount
ENV ARC_RELAY_DB_PATH=/data/arc-relay.db
ENV ARC_RELAY_MEMORY_DB_PATH=/data/memory.db

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
