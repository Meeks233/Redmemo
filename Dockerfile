FROM golang:1.26-alpine AS builder
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /redmemo ./cmd/redmemo


# Audio/video muxing is done in-process (internal/media: github.com/abema/go-mp4),
# so the runtime image no longer bundles a static ffmpeg binary.
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S redmemo && adduser -S redmemo -G redmemo
COPY --from=builder /redmemo /usr/local/bin/redmemo
COPY config.example.yaml /etc/redmemo/config.yaml
RUN mkdir -p /data/media && chown -R redmemo:redmemo /data/media
USER redmemo
EXPOSE 8080
ENTRYPOINT ["redmemo"]
CMD ["/etc/redmemo/config.yaml"]