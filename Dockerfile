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
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/codegate-server ./cmd/codegate-server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates su-exec \
    && addgroup -S -g 10001 codegate \
    && adduser -S -D -H -u 10001 -G codegate codegate \
    && mkdir -p /data \
    && chown codegate:codegate /data
COPY --from=build /out/codegate-server /usr/local/bin/codegate-server
COPY deploy/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 755 /usr/local/bin/docker-entrypoint.sh
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
EXPOSE 8080
VOLUME /data
CMD ["codegate-server", "serve"]
