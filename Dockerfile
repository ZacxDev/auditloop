# Multi-stage build. The runtime stage bundles Chromium so the worker role can
# crawl. Chromium is resolved at runtime via AUDITLOOP_CHROMIUM (set below) or
# the PATH.

# --- CSS build (Tailwind v4 CLI) ---
FROM node:22-alpine AS css
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm install
COPY static/input.css static/input.css
COPY components components
COPY handlers handlers
RUN ./node_modules/.bin/tailwindcss -i static/input.css -o static/output.css --minify

# --- Go build ---
FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=css /app/static/output.css static/output.css
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/auditloop .

# --- Runtime (Chromium-bearing) ---
FROM alpine:3.20
RUN apk add --no-cache chromium nss freetype harfbuzz ca-certificates ttf-freefont tzdata \
    && adduser -D -u 10001 auditloop
ENV AUDITLOOP_CHROMIUM=/usr/bin/chromium-browser
ENV AUDITLOOP_ROLE=all
WORKDIR /app
COPY --from=build /out/auditloop /app/auditloop
COPY static /app/static
USER auditloop
EXPOSE 8112
ENTRYPOINT ["/app/auditloop"]
