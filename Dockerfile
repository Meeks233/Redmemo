FROM golang:1.26-alpine AS builder
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /redmemo ./cmd/redmemo


# Download static ffmpeg
FROM alpine:3.21 AS ffmpeg-fetcher
RUN apk add --no-cache wget xz
RUN wget -qO /tmp/ffmpeg.tar.xz \
    https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-amd64-static.tar.xz \
    && tar -xf /tmp/ffmpeg.tar.xz -C /tmp \
    && mv /tmp/ffmpeg-*-amd64-static/ffmpeg /usr/local/bin/ffmpeg \
    && chmod +x /usr/local/bin/ffmpeg


FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S redmemo && adduser -S redmemo -G redmemo
COPY --from=ffmpeg-fetcher /usr/local/bin/ffmpeg /usr/local/bin/ffmpeg
COPY --from=builder /redmemo /usr/local/bin/redmemo
COPY config.example.yaml /etc/redmemo/config.yaml
RUN mkdir -p /data/media && chown -R redmemo:redmemo /data/media
USER redmemo
EXPOSE 8080
ENTRYPOINT ["redmemo"]
CMD ["/etc/redmemo/config.yaml"]