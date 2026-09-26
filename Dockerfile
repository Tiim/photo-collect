FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/photos ./cmd/photos

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/photos /photos
ENV DATABASE_PATH=/data/photos.db \
    STORAGE_PATH=/data/photos \
    EXPORT_DIR=/data/exports \
    LISTEN_ADDR=:8080
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/photos"]
