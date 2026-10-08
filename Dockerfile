# OpenMessage — headless MCP server
#
# Run the OpenMessage backend as a server, exposing the local message store
# and MCP endpoint to assistants. Useful when you want to keep the inbox
# running on a home server / NAS and connect from desktop clients.
#
# Build:   docker build -t openmessage .
# Run:     docker run -p 7007:7007 -v openmessage-data:/data openmessage
# Pair:    docker exec -it <container> openmessage pair
# Connect: claude mcp add -s user --transport sse openmessage http://<host>:7007/mcp/sse

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/openmessage \
      .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S openmessage && \
    adduser -S -G openmessage -h /home/openmessage openmessage && \
    mkdir -p /data && chown openmessage:openmessage /data
USER openmessage
WORKDIR /home/openmessage
COPY --from=build /out/openmessage /usr/local/bin/openmessage

ENV OPENMESSAGES_DATA_DIR=/data \
    OPENMESSAGES_HOST=0.0.0.0 \
    OPENMESSAGES_PORT=7007

VOLUME ["/data"]
EXPOSE 7007

# Default to running the server. Override with `pair`, `import`, etc.
ENTRYPOINT ["openmessage"]
CMD ["serve"]
