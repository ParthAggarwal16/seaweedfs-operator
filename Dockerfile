# Build the manager binary.
FROM golang:1.24 AS builder
ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /workspace

# Copy the module files first so dependency download is cached independently of
# the source, which is what keeps rebuilds fast.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# CGO_ENABLED=0 gives a static binary that runs on the distroless base below.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -a -ldflags="-s -w" -o manager ./cmd

# Distroless keeps the attack surface small: no shell, no package manager.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
