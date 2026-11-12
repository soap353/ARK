FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN go mod download || true
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /druglabels .

FROM alpine:3.20
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /druglabels /app/druglabels

EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/app/druglabels"]
