FROM golang:1.26 AS build
WORKDIR /src
ENV GOPROXY=https://proxy.golang.org,direct
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    for attempt in 1 2 3 4; do \
      go mod download && exit 0; \
      echo "go mod download failed (attempt ${attempt}/4), retrying..."; \
      sleep $((attempt * 2)); \
    done; exit 1
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/clashrulepilot ./cmd/clashrulepilot && \
    mkdir -p /out/app/data && touch /out/app/data/.keep

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/clashrulepilot /clashrulepilot
COPY --from=build /out/app /app
# Start as root only long enough for the Go entrypoint to chown DATA_DIR;
# it immediately drops to uid/gid 65532 before network services start.
USER root:root
ENTRYPOINT ["/clashrulepilot"]
