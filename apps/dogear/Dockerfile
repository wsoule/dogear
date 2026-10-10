# Multi-stage build: a static Go binary on a distroless base, running as non-root.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# BUILD_TAGS=staging compiles in /test/login for the red-team script.
# Production builds leave it empty so that endpoint does not exist.
ARG BUILD_TAGS=""
RUN CGO_ENABLED=0 go build -tags "${BUILD_TAGS}" -trimpath -ldflags="-s -w" -o /out/dogear ./cmd/dogear

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dogear /app/dogear
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/dogear"]
CMD ["serve"]
