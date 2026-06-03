# ---- Build stage ----
FROM golang:1.25-alpine AS build

# Version stamped into the binary via -ldflags (see Makefile). Passed in as a
# build arg because .git is outside the build context, so `git describe` can't
# run in here; the caller computes it (e.g. from `git describe --tags`) and
# hands it in. Defaults to "dev" so a plain build still works.
ARG VERSION=dev

RUN apk add --no-cache make git protobuf
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@latest && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

WORKDIR /src

# Download dependencies first (cached layer)
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Generate protobuf code and build static binary
RUN make proto
RUN CGO_ENABLED=0 make build VERSION=$VERSION

# ---- Runtime stage ----
FROM alpine:3.19

RUN apk add --no-cache ca-certificates

WORKDIR /app

# Copy binary from build stage
COPY --from=build /src/bin/lettuce-server .

# Copy migration files for debugging (embedded in binary, but useful for inspection)
COPY --from=build /src/migrations/ ./migrations/

# Copy default config
COPY lettuce.yaml.example lettuce.yaml

EXPOSE 8080 9090

ENTRYPOINT ["./lettuce-server", "--config", "lettuce.yaml"]
