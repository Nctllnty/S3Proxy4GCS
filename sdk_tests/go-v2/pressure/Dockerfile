# Go V2 pressure runner image for the DNS-cutting workflow.
# Runs inside the cluster via a K8s Job so the view of s3proxy.lb.local is
# the real internal-DNS view (ALB in Phase A, CNAME->GCS in Phase C).

# ---- Stage 1: Build ----
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download

COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /pressure .

# ---- Stage 2: Minimal runtime ----
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /pressure /pressure

ENTRYPOINT ["/pressure"]
