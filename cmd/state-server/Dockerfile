FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /state-server ./cmd/state-server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /state-server /state-server

ENV STATE_SERVER_HOST=0.0.0.0
ENV STATE_SERVER_PORT=7449

EXPOSE 7449

ENTRYPOINT ["/state-server"]
