# The TusK backup receiver: `berth receive`, the same binary as Berth itself, run as its own container.
# Build context is the repository root (see docker-compose.yml).

FROM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first so a source-only change does not download the module cache again.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Fully static (no cgo), so it runs in an image with no libc at all.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/berth ./cmd/berth

# The data directories are created here so their ownership is baked into the image. Docker copies that
# ownership onto a fresh named volume, which lets the receiver run as a non-root user and still write.
RUN mkdir -p /data/uploads /data/received && chown -R 65532:65532 /data


FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/berth /usr/local/bin/berth
COPY --from=build --chown=65532:65532 /data /data

# uploads/ and received/ live under one directory on purpose: placing a finished upload is a rename,
# instant within one filesystem and a full copy across two.
#
# The sender binaries are NOT in the image: they are built from the TusK repository, and the receiver
# hands them to the machines it backs up. Mount the folder that holds tusk-agent and tusk-send here.
ENV TUSK_UPLOAD_DIR=/data/uploads \
    TUSK_RECEIVED_DIR=/data/received \
    TUSK_CLIENT_BINARY=/usr/local/share/tusk/tusk-send \
    TUSK_AGENT_BINARY=/usr/local/share/tusk/tusk-agent \
    TUSK_PORT=5183

USER nonroot:nonroot
WORKDIR /data
EXPOSE 5183

# No shell in this image, so the preflight checks inside the binary are the health gate: it refuses to
# start on an unwritable volume or a disk already below its reserve.
ENTRYPOINT ["/usr/local/bin/berth", "receive"]
