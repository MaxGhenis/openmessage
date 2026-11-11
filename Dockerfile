# OpenMessage — headless MCP server
#
# Run the OpenMessage backend as a server, exposing the local message store
# and MCP endpoint to assistants. Useful when you want to keep the inbox
# running on a home server / NAS and connect from desktop clients.
#
# Build:   docker build -t openmessage .
# Run:     docker run -p 127.0.0.1:7007:7007 -v openmessage-data:/data openmessage
# Pair:    docker exec -it <container> openmessage pair
#          (restart the server afterward to load the new session)
#          (to re-pair, stop the container and run pair in a fresh one with the same volume)
# Connect: claude mcp add -s user --transport sse openmessage http://127.0.0.1:7007/mcp/sse

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/openmessage \
      .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S -g 101 openmessage && \
    adduser -S -u 100 -G openmessage -h /home/openmessage openmessage && \
    mkdir -p /data && \
    chown 100:101 /data
USER 100:101
WORKDIR /home/openmessage
COPY --from=build /out/openmessage /usr/local/bin/openmessage

ENV HOME=/home/openmessage \
    OPENMESSAGES_DATA_DIR=/data \
    OPENMESSAGES_HOST=0.0.0.0 \
    OPENMESSAGES_PORT=7007

VOLUME ["/data"]
EXPOSE 7007

# Default to running the server. Override with `pair`, `import`, etc.
ENTRYPOINT ["openmessage"]
CMD ["serve", "--web", "--mcp-sse"]
