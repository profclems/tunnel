FROM golang:1.23-alpine AS builder

ARG VERSION=dev

WORKDIR /app

RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o /tunnel ./cmd/tunnel

FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /tunnel /tunnel

ENTRYPOINT ["/tunnel"]
