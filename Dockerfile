FROM golang:1.25.7-alpine3.22 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY internal ./internal

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/claude-codex-proxy .

FROM alpine:3.22

RUN apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S -G app -h /home/app app \
    && mkdir -p /app /home/app \
    && chown -R app:app /app /home/app

ENV HOME=/home/app \
    CLAUDE_CODE_PROXY_LISTEN_ADDR=0.0.0.0:8787
WORKDIR /app

COPY --from=build /out/claude-codex-proxy /usr/local/bin/claude-codex-proxy
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh

USER app:app

EXPOSE 8787

ENTRYPOINT ["docker-entrypoint.sh"]
