# 构建阶段
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nbco ./cmd/nbco

# 运行阶段。容器部署适用 eino 引擎（直调 API）；
# CLI 引擎（claudecli/codexcli）依赖宿主机安装的 CLI 与其登录态，建议裸机部署。
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/nbco /usr/local/bin/nbco
ENTRYPOINT ["nbco"]
CMD ["-config", "/etc/nbco/nbco.json"]
