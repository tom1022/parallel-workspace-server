# Workspace Controller. Static Go binary on a distroless base: no shell, no
# package manager, nothing beyond the binary and CA certs it needs to reach
# the Kubernetes API server.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/controller .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/controller /controller
USER nonroot:nonroot
ENTRYPOINT ["/controller"]
