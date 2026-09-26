FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# nodynamic: stop purego (via gen2brain/heic) from dynamically linking libc, which Alpine lacks (glibc loader)
RUN CGO_ENABLED=0 go build -tags nodynamic -trimpath -ldflags="-s -w" -o /out/photos ./cmd/photos

FROM alpine:3.22
RUN addgroup -g 65532 -S photos \
 && adduser -u 65532 -S -G photos -H -s /sbin/nologin photos \
 && mkdir /data && chown photos:photos /data
COPY --from=build /out/photos /photos
ENV DATABASE_PATH=/data/photos.db \
    STORAGE_PATH=/data/photos \
    EXPORT_DIR=/data/exports \
    LISTEN_ADDR=:8080
VOLUME /data
EXPOSE 8080
USER photos:photos
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/photos", "healthcheck"]
ENTRYPOINT ["/photos"]
