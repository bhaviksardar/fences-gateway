# Multi-arch: docker buildx build --platform linux/amd64,linux/arm64 .
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Pure Go (modernc SQLite), so it cross-compiles without cgo
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o /fences-gateway ./cmd/fences-gateway

# No shell, no package manager; runs as uid 65532
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /fences-gateway /fences-gateway
EXPOSE 8080
ENTRYPOINT ["/fences-gateway"]
CMD ["-config", "/etc/fences-gateway/config.json"]
