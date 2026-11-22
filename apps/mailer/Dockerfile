# syntax=docker/dockerfile:1

# ---- 阶段 1：构建二进制 ----
FROM golang:1.26-alpine AS builder
WORKDIR /build

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/mailer .

# ---- 阶段 2：生产镜像 ----
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /out/mailer /app/mailer

EXPOSE 8003

ENV MAILER_PORT=8003 \
    APP_HOST=0.0.0.0

CMD ["/app/mailer"]
