# Build stage.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Download dependencies first so this layer caches independently of the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/gateway ./cmd/gateway

# Runtime stage: distroless, non-root, no shell.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/gateway /usr/local/bin/gateway
COPY --from=build /src/config/gateway.example.yaml /etc/ai-gateway/gateway.example.yaml

USER nonroot:nonroot
EXPOSE 4000

ENTRYPOINT ["/usr/local/bin/gateway"]
CMD ["-config", "/etc/ai-gateway/gateway.yaml"]
