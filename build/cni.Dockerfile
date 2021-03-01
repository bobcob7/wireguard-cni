FROM golang:latest as builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o wg-cni ./cmd/wg-cni

FROM alpine:3.11

RUN apk add --no-cache bash
COPY --from=builder app/wg-cni /opt/cni/bin/wg-cni
COPY scripts/install /install

ENTRYPOINT ["/install"]
