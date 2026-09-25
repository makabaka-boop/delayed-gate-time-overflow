# 多阶段构建：logic 服务（无 CGO，静态二进制）
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/logic ./cmd/logic

FROM alpine:3.20
RUN adduser -D -u 10001 appuser
COPY --from=build /out/logic /usr/local/bin/logic
USER appuser
EXPOSE 8080
ENV LOGIC_ADDR=:8080
ENTRYPOINT ["logic"]
