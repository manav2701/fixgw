# syntax=docker/dockerfile:1

# Build on the host's own architecture and cross-compile, so building the arm64
# image (Oracle's free Ampere VMs) needs no emulation. The binaries are pure Go:
# SQLite is modernc.org/sqlite, not cgo.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/fixgw ./cmd/fixsim ./cmd/fixbt
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/ /app/
COPY spec/FIX44.xml /app/spec/FIX44.xml
COPY deploy/ /app/deploy/
# State (sequence-number store, audit log) lives on a volume, owned by the
# unprivileged user.
COPY --from=build --chown=65532:65532 /data /data
VOLUME /data
EXPOSE 5001 8080
ENTRYPOINT ["/app/fixgw"]
CMD ["-config=/app/deploy/fixgw.docker.cfg", "-creds=/app/deploy/credentials.example.json", \
     "-risk=/app/deploy/risk.json", "-db=/data/fixgw.db", "-audit=/data/audit.jsonl", "-http=0.0.0.0:8080"]
