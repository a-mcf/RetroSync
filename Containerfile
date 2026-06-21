# syntax=docker/dockerfile:1
# Multi-stage build: static binary on golang:1.23, distroless runtime.

FROM golang:1.23 AS builder
WORKDIR /src

# Cache modules first.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/retrosync ./cmd/retrosync

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/retrosync /usr/local/bin/retrosync
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/retrosync"]
