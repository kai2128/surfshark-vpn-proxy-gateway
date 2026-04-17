FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /gateway ./cmd/gateway

FROM alpine:3.20

RUN apk add --no-cache \
	openvpn \
	iproute2 \
	iptables \
	ca-certificates

COPY --from=builder /gateway /usr/local/bin/gateway

EXPOSE 1080 8888

ENTRYPOINT ["/usr/local/bin/gateway"]
