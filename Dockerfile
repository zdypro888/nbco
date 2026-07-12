# 构建阶段
FROM golang:1.26.5-alpine3.24 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/nbco ./cmd/nbco

# 运行阶段。中枢只运行 eino API 引擎；AI 员工请把 nbco-worker 装到真实工作机。
FROM alpine:3.24.1
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/nbco /usr/local/bin/nbco
ENTRYPOINT ["nbco"]
CMD ["-config", "/etc/nbco/nbco.json"]
