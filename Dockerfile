# exe-hub in a container (PLAN.md, Hub identity & deployment: Docker
# Compose). A static Go build, then the smallest image that also carries
# ffmpeg for /v1/media; compose.yaml runs it beside kubo. The config it
# runs by default is docker/config.json; mount your own over
# /etc/exe-hub/config.json.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /exe-hub ./cmd/exe-hub

FROM alpine:3.24
# ffmpeg converts video and sound (x264 on the CPU: there is no GPU in
# here); the certificates are for the Solana RPC, link cards, the
# Wayback Machine and Ollama over HTTPS
RUN apk add --no-cache ffmpeg ca-certificates \
 && adduser -D -u 1000 -h /var/lib/exe-hub hub \
 && mkdir -p /etc/exe-hub
COPY --from=build /exe-hub /usr/local/bin/exe-hub
COPY --chmod=644 docker/config.json /etc/exe-hub/config.json
USER hub
WORKDIR /var/lib/exe-hub
# the state: hub.db, the hub's identity and push key, staged media. The
# variable is what -state would say, so `exe-hub -s reload` and the
# other side commands find the daemon's files in an exec too.
ENV EXE_HUB_STATE=/var/lib/exe-hub
VOLUME /var/lib/exe-hub
EXPOSE 7788
ENTRYPOINT ["exe-hub", "-config", "/etc/exe-hub/config.json"]
