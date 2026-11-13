# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS deps
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0

FROM deps AS build-api
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# Load tester + end-to-end test binary. Unit tests run as part of this build.
FROM deps AS build-tools
RUN go vet ./... && go test ./internal/...
RUN go build -trimpath -ldflags="-s -w" -o /out/loadtest ./cmd/loadtest \
 && go test -c -o /out/e2e.test ./e2e

FROM gcr.io/distroless/static-debian12:nonroot AS tools
COPY --from=build-tools /out/ /app/
ENTRYPOINT []

# Default (last) stage: the API image.
FROM gcr.io/distroless/static-debian12:nonroot AS api
COPY --from=build-api /out/api /app/api
EXPOSE 8080
ENTRYPOINT ["/app/api"]
