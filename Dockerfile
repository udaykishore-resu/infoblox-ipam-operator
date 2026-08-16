# syntax=docker/dockerfile:1

FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/operator ./cmd/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/mock-infoblox ./cmd/mock-infoblox

# --- operator: minimal, non-root, distroless-style runtime image ---
FROM gcr.io/distroless/static-debian12:nonroot AS operator
COPY --from=build /out/operator /operator
USER 65532:65532
ENTRYPOINT ["/operator"]

# --- mock-infoblox: same base, only used for local/demo/CI, never prod ---
FROM gcr.io/distroless/static-debian12:nonroot AS mock-infoblox
COPY --from=build /out/mock-infoblox /mock-infoblox
USER 65532:65532
EXPOSE 9090
ENTRYPOINT ["/mock-infoblox"]
