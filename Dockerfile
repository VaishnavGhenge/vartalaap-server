# Multi-stage build for the Sessionly signaling + API server.
#
# Static binary in a distroless base: no shell, no package manager, ~15 MB
# final image. Matters for cold-start and for the memory ceiling this service
# has historically run under (see project deployment constraints).
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependency layer first so a source-only change doesn't re-download modules.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off gives a fully static binary that runs in distroless/static.
# -trimpath keeps build paths out of the binary; -s -w drop the symbol table.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" \
    -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot

# CA certificates come with the distroless base and are required for outbound
# TLS to Cloudflare, Google, and Resend.
COPY --from=build /out/server /server

USER nonroot:nonroot

# Railway injects PORT; config.Load defaults to 8080 when it is absent.
EXPOSE 8080

ENTRYPOINT ["/server"]
