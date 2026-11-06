# syntax=docker/dockerfile:1

FROM node:24-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY ["cmd/", "./cmd/"]
COPY internal/ ./internal/
COPY --from=web /src/web/build/ ./internal/web/dist/
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/whatnow ./cmd/whatnow

FROM ubuntu:24.04 AS runtime
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl dbus gnome-keyring libsecret-tools tini tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 whatnow \
    && useradd --uid 10001 --gid whatnow --home-dir /data --no-create-home whatnow \
    && install -d -o whatnow -g whatnow -m 0700 /data
COPY --from=build /out/whatnow /usr/local/bin/whatnow
COPY --chmod=755 docker/entrypoint.sh /usr/local/bin/docker-entrypoint
ENV HOME=/data \
    XDG_DATA_HOME=/data/.local/share \
    XDG_RUNTIME_DIR=/tmp/whatnow-runtime \
    TZ=Etc/UTC
USER whatnow
WORKDIR /data
EXPOSE 7777
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD curl --fail --silent --output /dev/null http://127.0.0.1:7777/api/v1/settings || exit 1
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "dbus-run-session", "--", "/usr/local/bin/docker-entrypoint"]
CMD ["whatnow", "serve", "-addr", "0.0.0.0:7777", "-db", "/data/whatnow.db"]
