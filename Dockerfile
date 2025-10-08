ARG GO_VERSION=1.24

FROM golang:${GO_VERSION} AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 \
    go build -trimpath -ldflags='-s -w' -o /out/proxctl ./cmd

FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=builder /out/proxctl /usr/local/bin/proxctl

ENTRYPOINT ["/usr/local/bin/proxctl"]
