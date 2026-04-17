FROM golang:1.25.7-alpine3.22 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/claude-codex-proxy ./cmd/claude-codex-proxy

FROM alpine:3.22

RUN apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S -G app -h /home/app app \
    && mkdir -p /app /home/app \
    && chown -R app:app /app /home/app

ENV HOME=/home/app
WORKDIR /app

COPY --from=build /out/claude-codex-proxy /usr/local/bin/claude-codex-proxy

USER app:app

EXPOSE 8787

ENTRYPOINT ["claude-codex-proxy"]
