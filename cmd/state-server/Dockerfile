FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Build WASM SDK (inline make build-wasm since alpine lacks make)
RUN GOOS=js GOARCH=wasm go build -tags nethttpomit,osusergo,netgo -o cmd/wasm-sdk/quoter.wasm ./cmd/wasm-sdk
RUN cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" cmd/wasm-sdk/wasm_exec.js
RUN cp cmd/wasm-sdk/quoter.wasm cmd/state-server/sdk/quoter.wasm
RUN cp cmd/wasm-sdk/wasm_exec.js cmd/state-server/sdk/wasm_exec.js

# Build state server (with WASM embedded)
RUN CGO_ENABLED=0 go build -o /state-server ./cmd/state-server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /state-server /state-server

ENV STATE_SERVER_HOST=0.0.0.0
ENV STATE_SERVER_PORT=7449

EXPOSE 7449

ENTRYPOINT ["/state-server"]
