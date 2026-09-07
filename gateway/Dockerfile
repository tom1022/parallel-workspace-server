# Terminal Gateway. Static Go binary on a distroless base: no shell, no package
# manager, nothing beyond the binary and the CA certs it needs to reach the
# Kubernetes API server and the authorization server's JWKS endpoint.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /gateway
USER nonroot:nonroot
ENTRYPOINT ["/gateway"]
