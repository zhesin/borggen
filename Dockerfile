# syntax=docker/dockerfile:1

# --- build ------------------------------------------------------------------
FROM golang:1.24-bookworm AS build
WORKDIR /src

# Dependencies in their own layer, before the source: go-git pulls in 22
# transitive modules, and with a single `COPY . .` any edit at all — a README
# typo, a line of CSS — invalidated the layer and re-downloaded every one of
# them. This layer is rebuilt only when go.mod/go.sum actually change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/borggen .

# --- test -------------------------------------------------------------------
# Separate target so the suite can be run reproducibly:
#   docker build --target test .
# bash lives in the builder image, which is what `bash -n` needs.
FROM build AS test
RUN go vet ./... && go test ./...

# --- runtime ----------------------------------------------------------------
FROM alpine:3.20
RUN adduser -D -u 10001 borggen && mkdir -p /data && chown borggen:borggen /data
COPY --from=build /out/borggen /usr/local/bin/borggen
USER borggen
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/borggen"]
CMD ["--addr", ":8080", "--data", "/data"]
