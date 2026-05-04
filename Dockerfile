# Build stage — compile a static Go binary
FROM golang:1.24-bookworm AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /ec2cp .

# Run stage — minimal image with the binary + instances.json
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /ec2cp /usr/local/bin/ec2cp
COPY instances.json /app/instances.json
WORKDIR /app
EXPOSE 2720
CMD ["/usr/local/bin/ec2cp", "serve", "--port", "2720"]
