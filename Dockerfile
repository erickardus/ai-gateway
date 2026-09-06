# UI stage. The admin console is a Vite build that internal/ui embeds into the
# binary, so it has to be produced before the Go build rather than shipped
# beside it.
FROM node:22-alpine AS ui

WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Build stage.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Download dependencies first so this layer caches independently of the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Overlay the built assets so go:embed picks them up. The directory in the
# source tree holds only a .gitkeep.
COPY --from=ui /src/internal/ui/dist ./internal/ui/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/gateway ./cmd/gateway

# Runtime stage: distroless, non-root, no shell.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/gateway /usr/local/bin/gateway
# Shipped as gateway.yaml so the default CMD works out of the box. Mount your
# own over /etc/ai-gateway/gateway.yaml to override it:
#   docker run -v ./gateway.yaml:/etc/ai-gateway/gateway.yaml ai-gateway
COPY --from=build /src/config/gateway.example.yaml /etc/ai-gateway/gateway.yaml

USER nonroot:nonroot
EXPOSE 4000

ENTRYPOINT ["/usr/local/bin/gateway"]
CMD ["-config", "/etc/ai-gateway/gateway.yaml"]
