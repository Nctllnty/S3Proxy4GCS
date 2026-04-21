# ---- Stage 1: Build benchmark test binary ----
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETOS TARGETARCH

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Compile the test binary (includes all *_test.go files)
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go test -c -o /benchmark -trimpath -ldflags="-s -w" ./...

# ---- Stage 2: Minimal runtime ----
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /benchmark /benchmark

ENTRYPOINT ["/benchmark"]
