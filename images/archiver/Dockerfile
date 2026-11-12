FROM alpine:3.20

RUN apk add --no-cache \
    python3 \
    py3-pip \
    ffmpeg \
    aria2 \
    curl \
    bash \
    ca-certificates

# Install latest yt-dlp binary
RUN curl -L https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp -o /usr/local/bin/yt-dlp \
    && chmod a+rx /usr/local/bin/yt-dlp

WORKDIR /data

ENTRYPOINT ["/bin/bash"]
