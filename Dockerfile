# Build stage — Go version declared here must match go.mod (challenge §4).
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Build identity, injected into internal/platform/buildinfo at link time.
# Local `docker build` leaves them at their honest defaults ("dev"/"none");
# the release workflow passes the real tag, commit and timestamp so a shipped
# image reports its own version via `walletd version`. VERSION is the only one
# with a meaningful default because it is the one a human reads.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN go build -trimpath \
        -ldflags="-s -w \
          -X github.com/chalkan3/backend-challenge-go/internal/platform/buildinfo.Version=${VERSION} \
          -X github.com/chalkan3/backend-challenge-go/internal/platform/buildinfo.Commit=${COMMIT} \
          -X github.com/chalkan3/backend-challenge-go/internal/platform/buildinfo.Date=${DATE}" \
        -o /out/walletd ./cmd/walletd

# Runtime stage — minimal image with a non-root user and curl for healthchecks.
FROM alpine:3.21
RUN adduser -D -u 10001 app && apk add --no-cache ca-certificates curl
COPY --from=build /out/walletd /usr/local/bin/walletd
USER app
EXPOSE 8080
ENTRYPOINT ["walletd"]
