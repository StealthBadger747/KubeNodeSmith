ARG GO_VERSION=1.24

FROM golang:${GO_VERSION} AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 \
    go build -trimpath -ldflags='-s -w' -o /out/nodesmith ./cmd/nodesmith.go

FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=builder /out/nodesmith /usr/local/bin/nodesmith

ENTRYPOINT ["/usr/local/bin/nodesmith"]
