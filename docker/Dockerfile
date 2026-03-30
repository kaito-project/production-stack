# Build stage
FROM golang:1.26 AS builder
WORKDIR /workspace

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Build
COPY cmd/ cmd/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o gpu-node-mocker ./cmd/gpu-node-mocker

# Runtime stage
FROM gcr.io/distroless/static:nonroot 
WORKDIR /
COPY --from=builder /workspace/gpu-node-mocker .
USER 65532:65532
ENTRYPOINT ["/gpu-node-mocker"]
