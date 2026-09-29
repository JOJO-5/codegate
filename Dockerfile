# Build the SPA first; it is embedded into the Server binary by go:embed.
FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist/ ./internal/server/webui/dist/
RUN set -eu; version=$(cat VERSION); \
    case "$version" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "invalid VERSION: $version" >&2; exit 1;; esac; \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=$version" -o /out/codegate-server ./cmd/codegate-server; \
    for goos in linux darwin windows; do \
      for goarch in amd64 arm64; do \
        dir="/out/agent-updates/$goos/$goarch"; mkdir -p "$dir"; \
        name=codegate-agent; if [ "$goos" = windows ]; then name=codegate-agent.exe; fi; \
        GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$version" -o "$dir/$name" ./cmd/codegate-agent; \
        printf '%s\n' "$version" > "$dir/version.txt"; \
      done; \
    done

FROM alpine:3.22
RUN apk add --no-cache ca-certificates su-exec \
    && addgroup -S -g 10001 codegate \
    && adduser -S -D -H -u 10001 -G codegate codegate \
    && mkdir -p /data \
    && chown codegate:codegate /data
COPY --from=build /out/codegate-server /usr/local/bin/codegate-server
COPY --from=build /out/agent-updates/ /opt/codegate/agent-updates/
COPY deploy/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 755 /usr/local/bin/docker-entrypoint.sh
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
EXPOSE 8080
VOLUME /data
CMD ["codegate-server", "serve"]
