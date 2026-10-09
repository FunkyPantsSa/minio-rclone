# deploy/Dockerfile —— 备份控制台多阶段构建（一个镜像 = 控制台 + rclone）
#
# 构建命令（在项目根目录 backup-controller/ 下执行，Dockerfile 位于 deploy/）：
#   docker build -f deploy/Dockerfile -t backup-controller:latest .
#   docker tag backup-controller:latest harbor.internal/base/backup-controller:latest
#   docker push harbor.internal/base/backup-controller:latest
#
# 设计说明（独立运行模式：web + rclone 单进程组合）：
#   - 最终镜像同时包含：备份控制台二进制 + rclone 二进制（固定版本 1.69.0），
#     一个镜像即可完成部署与备份执行：
#       1) 控制台通过 ENTRYPOINT 启动，监听 8000；
#       2) 备份任务由控制台直接 exec 容器内的 rclone（RCLONE_BIN 默认 PATH 中的 rclone）。
#   - 前端 HTML 通过 go:embed 编译进二进制，运行镜像无需携带任何静态文件
#   - SQLite 为纯 Go 驱动（modernc.org/sqlite），二进制为静态编译（CGO_ENABLED=0）
#   - 运行镜像基于 alpine，自带 ca-certificates
#   - 需要挂载：备份目标目录（BACKUP_DEST_DIR）与运行数据目录（DATA_DIR，存放 SQLite 与日志）

# ---------- 阶段一：编译 Go 二进制 ----------
FROM golang:1.22-alpine AS builder
WORKDIR /src

# 先拷贝依赖清单以利用 Docker 构建缓存
COPY go.mod go.sum ./
RUN go mod download

# 再拷贝全部源码（含 templates/，go:embed 需要）
COPY . .

# CGO_ENABLED=0 静态编译（modernc.org/sqlite 纯 Go，无需 CGO），便于放入精简运行镜像
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /backup-controller .

# ---------- 阶段二：运行镜像（控制台 + rclone） ----------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app

# 从官方 rclone 镜像复制固定版本的 rclone 二进制（静态编译，兼容 alpine）
# 官方镜像路径为 /usr/local/bin/rclone（见 rclone 官方 Dockerfile）
COPY --from=rclone/rclone:1.69.0 /usr/local/bin/rclone /usr/local/bin/rclone

USER app
COPY --from=builder /backup-controller /usr/local/bin/backup-controller
EXPOSE 8000
ENTRYPOINT ["/usr/local/bin/backup-controller"]
