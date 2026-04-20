# ---- Stage 1: Build ----
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETOS TARGETARCH

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /s3proxy main.go

# ---- Stage 2: Runtime ----
FROM alpine:3.20

RUN apk add --no-cache ca-certificates bash curl tzdata \
    && addgroup -S app && adduser -S app -G app

COPY --from=builder /s3proxy /usr/local/bin/s3proxy

USER app
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/s3proxy"]
