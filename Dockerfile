# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
# go.sum 必须与 go.mod 同层：只 COPY go.mod 时 `go mod download` 那层拿不到
# 校验和，go.sum 要到 `COPY . .` 才进来——依赖层缓存因此失去意义（改 go.sum
# 会重下全部依赖），且校验推迟到构建之后。
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/buddyhub ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit

FROM alpine:3.20
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/buddyhub /app/buddyhub
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY login.sh signin.sh credit.sh /app/
COPY scripts/probe_active.py /app/scripts/probe_active.py
RUN sed -i 's/\r$//' /app/login.sh /app/signin.sh /app/credit.sh && chmod 755 /app/login.sh /app/signin.sh /app/credit.sh
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）。
# 必须 --chown 给 app：首启会把 example 里的占位 api_key 换成随机密钥并写回该
# 文件（cmd/server/config.go: rotateExampleKey），root 所有的 0644 文件写不回去
# 就只能带着公开可预测的密钥跑起来。
COPY --chown=app:app config.example.json /app/config.json
USER app
EXPOSE 7863
# 探活必须接受 503。/healthz 的判活口径是「池里有可服务账号」，而新装的容器
# 池子是空的（handler.go 无账号 → 503，README 也写明 503 属正常初态）。用
# `wget -qO-` 会在 503 上退出非零（busybox=1 / GNU=8），于是每个新人第一次
# docker run 都在 docker ps 里看到 (unhealthy)，配了自动重建的编排还会反复重启它。
# 这里只区分「进程在答 HTTP」与「连不上」：200/503 记健康，拒连/超时记不健康。
# 用 python3 而不是 wget，是因为要读状态码而不是让工具替它决定成败；镜像已装。
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD python3 -c "import http.client as c,sys; s=c.HTTPConnection('127.0.0.1',7863,timeout=4); s.request('GET','/healthz'); r=s.getresponse(); r.read(); sys.exit(0 if r.status in (200,503) else 1)"
ENTRYPOINT ["/app/buddyhub", "-config", "/app/config.json"]