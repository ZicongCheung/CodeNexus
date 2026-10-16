# syntax=docker/dockerfile:1

# --- stage 1: build the panel ---
FROM node:22-alpine AS web
WORKDIR /web
# Copy the manifest and lockfile first so the dependency layer is cached until
# the dependencies themselves change; `npm ci` is the reproducible install the
# build script uses too.
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build -- --outDir /dist-panel

# --- stage 2: build the binary ---
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
COPY --from=web /dist-panel ./internal/panel/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/codenexus ./cmd/codenexus

# --- stage 3: runtime ---
FROM alpine:3.20
RUN adduser -D -u 10001 app \
    && apk add --no-cache ca-certificates tzdata wget
WORKDIR /app
COPY --from=build /out/codenexus /app/codenexus
RUN mkdir -p /app/auths /app/data && chown -R app:app /app
USER app
EXPOSE 6828
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:6828/healthz || exit 1
ENTRYPOINT ["/app/codenexus", "-config", "/app/config.json"]
