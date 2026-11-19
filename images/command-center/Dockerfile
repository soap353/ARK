FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata docker-cli

WORKDIR /app
COPY images/command-center/ark-linux /app/ark
COPY catalog /app/catalog
COPY images /app/images
RUN chmod +x /app/ark

EXPOSE 8080
ENTRYPOINT ["/app/ark", "run", "-foreground", "-no-browser", "-port", "8080", "-data-dir", "/data"]
