# Build stage — Go version declared here must match go.mod (challenge §4).
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/walletd ./cmd/walletd

# Runtime stage — minimal image with a non-root user and curl for healthchecks.
FROM alpine:3.21
RUN adduser -D -u 10001 app && apk add --no-cache ca-certificates curl
COPY --from=build /out/walletd /usr/local/bin/walletd
USER app
EXPOSE 8080
ENTRYPOINT ["walletd"]
