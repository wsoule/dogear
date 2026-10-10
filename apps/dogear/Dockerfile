# Multi-stage build: a static Go binary on a small Alpine base (CA certs for
# GitHub and OAuth calls), running as a non-root user. There is no ENTRYPOINT
# so each Railway service can set its own start command, e.g.
# "/app/dogear import" for the weekly importer cron.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# BUILD_TAGS=staging compiles in /test/login for the red-team script.
# Production builds leave it empty so that endpoint does not exist.
ARG BUILD_TAGS=""
RUN CGO_ENABLED=0 go build -tags "${BUILD_TAGS}" -trimpath -ldflags="-s -w" -o /out/dogear ./cmd/dogear

FROM alpine:3
RUN apk add --no-cache ca-certificates && adduser -D -H -u 10001 dogear
COPY --from=build /out/dogear /app/dogear
USER dogear
EXPOSE 8080
CMD ["/app/dogear", "serve"]
