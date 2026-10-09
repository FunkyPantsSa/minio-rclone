# MinIO 备份控制台（Golang，web + rclone 独立模式）

基于 `rclone` 的 MinIO → 本地磁盘备份**全流程管理工具**。

**架构（v2）**：不再经过 Kubernetes（不创建 Job/CronJob），由 Web 应用**直接执行 rclone 进程**完成备份，
并内置定时调度器；任务记录与运行数据持久化在 **SQLite**（可挂载目录存放），页面实时展示进度。
一个进程 = Web 管理界面 + 备份执行器 + 定时调度器，方便管理、随处可跑（主机 / docker / K8s 普通容器均可）。

## 功能

- **全量备份**：手动触发一次 `rclone copy`（不设超时），与增量互斥
- **增量备份**：内置定时调度（默认每 10 分钟）+ 手动触发；重叠调度自动跳过（等价 Forbid）
- **实时进度**：解析 rclone `--use-json-log` 的 stats 行，页面实时显示百分比/速度/文件数（不再依赖轮询 Pod 日志）
- **任务历史**：SQLite 持久化所有任务（类型/状态/起止时间/退出码/最终进度），支持删除与按保留数自动清理
- **实时日志**：SSE 流式查看任务日志（日志以文件形式存放于 DATA_DIR/logs）
- **定时调度管理**：面板展示计划/上次/下次执行/历史保留，一键暂停/恢复（状态持久化，重启不丢失）
- **配置面板**：页面查看全部生效配置（目录/桶/并发/带宽/计划/凭证状态等）
- **登录认证**：固定账号密码（环境变量配置），HMAC 签名会话 Cookie

## 目录结构

```
minio-rclone/
├── main.go              # 入口：配置加载、SQLite 初始化、运行器/调度器启动、HTTP 服务
├── config.go            # 全部可配置项（环境变量驱动，见 configmap.yaml）
├── db.go                # SQLite 持久化（任务记录 + 设置项）
├── runner.go            # 任务运行器：直接 exec rclone、实时进度、任务互斥、内置定时调度
├── handlers.go          # API 处理函数（含 go:embed 前端页面）
├── progress.go          # rclone JSON 进度解析（单行/多行）
├── auth.go              # 登录认证（HMAC 签名会话 Cookie）
├── progress_test.go / auth_test.go   # 单元测试
├── templates/
│   ├── index.html       # 主页面（编译期嵌入二进制）
│   └── login.html       # 登录页（编译期嵌入二进制）
├── go.mod / go.sum      # 依赖（robfig/cron/v3 调度、modernc.org/sqlite 持久化）
└── deploy/
    ├── namespace.yaml    # middleware 命名空间
    ├── configmap.yaml    # rclone.conf + backup-controller-config（全部配置项）
    ├── secret.yaml       # AK/SK（部署时以环境变量注入）
    ├── deployment.yaml   # K8s 普通容器部署（可选：也可以 docker run 或直接跑在主机）
    └── Dockerfile        # 多阶段构建（一个镜像 = 控制台 + rclone）
```

## 配置（全部环境变量化）

见 `deploy/configmap.yaml` 的 `backup-controller-config`。常用项：

| 配置项 | 说明 |
|--------|------|
| `RCLONE_BIN` / `RCLONE_CONFIG_PATH` | rclone 可执行文件 / rclone.conf 路径（文件不存在自动跳过 --config） |
| `RCLONE_REMOTE` / `RCLONE_BUCKET` / `RCLONE_SOURCE_SUBDIR` | 远程名 / 桶名 / 桶内子目录 |
| `BACKUP_DEST_DIR` / `DATA_DIR` | 备份目标目录 / 运行数据目录（SQLite + 日志） |
| `RCLONE_BW_LIMIT` / `RCLONE_TRANSFERS` / `RCLONE_CHECKERS` / `RCLONE_RETRIES` | 带宽与并发 |
| `RCLONE_STATS_INTERVAL` / `RCLONE_USE_JSON_LOG` | 进度输出间隔（页面实时进度依赖 JSON 统计行） |
| `CRON_SCHEDULE` / `CRON_SUSPEND` / `INCREMENTAL_MAX_DURATION` | 定时计划 / 初始暂停 / 单次增量时长 |
| `SUCCESSFUL_HISTORY_LIMIT` / `FAILED_HISTORY_LIMIT` | 历史保留数（自动清理） |
| `LOGIN_USERNAME` / `LOGIN_PASSWORD` / `AUTH_SECRET` | 登录账号密码 / 会话签名密钥 |

凭证：`rclone.conf` 使用 `env_auth = true`，AK/SK 通过环境变量
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` 注入（K8s 部署时来自 `deploy/secret.yaml`，
docker run 时用 `-e` 传入），不落盘、不进命令行。

## 快速开始

### 方式一：直接本地/主机运行（需安装 rclone）

```bash
# 准备 rclone.conf（env_auth=true，endpoint 指向 MinIO）
# 然后：
RCLONE_BUCKET=my-bucket \
AWS_ACCESS_KEY_ID=xxx AWS_SECRET_ACCESS_KEY=yyy \
DATA_DIR=./data BACKUP_DEST_DIR=./backup \
go run .
```

打开 http://localhost:8000（默认账号 `admin / admin123`，**上线前修改 LOGIN_PASSWORD**）。

### 方式二：docker run（单镜像，含 rclone）

```bash
mkdir -p /data/mc_backup /data/bc
docker run -d --name backup-controller -p 8000:8000 \
  -v /data/mc_backup:/data \          # 备份落盘目录
  -v /data/bc:/data/bc \              # SQLite + 日志
  -e RCLONE_BUCKET=my-bucket \
  -e AWS_ACCESS_KEY_ID=xxx -e AWS_SECRET_ACCESS_KEY=yyy \
  harbor.internal/base/backup-controller:latest
```

### 方式三：Kubernetes 普通容器（不创建任何 K8s 资源）

```bash
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/secret.yaml        # 先替换 YOUR_ACCESS_KEY / YOUR_SECRET_KEY
kubectl apply -f deploy/configmap.yaml     # 先替换 YOUR_BUCKET 及其它配置
kubectl apply -f deploy/deployment.yaml    # 先替换镜像地址
# 访问：kubectl port-forward svc/backup-controller -n middleware 8080:80
```

## 构建镜像

```bash
cd minio-rclone
docker build -f deploy/Dockerfile -t backup-controller:latest .
docker tag backup-controller:latest harbor.internal/base/backup-controller:latest
docker push harbor.internal/base/backup-controller:latest
```

## API 一览

| 方法 | 路径 | 功能 |
|------|------|------|
| GET | `/login` | 登录页面 |
| POST | `/api/login` / `/api/logout` | 登录 / 退出登录 |
| GET | `/` | 前端 HTML 页面 |
| POST | `/api/full-backup` | 启动全量备份 |
| POST | `/api/incremental-backup` | 手动触发增量备份 |
| POST | `/api/cronjob/suspend` | `{"suspend": true/false}` 暂停/恢复定时调度 |
| GET | `/api/cronjob/status` | 定时调度详情（计划/上次/下次执行/历史保留） |
| GET | `/api/tasks` | 任务列表（含最终进度） |
| GET | `/api/tasks/{name}/logs?tail=200` | 静态最近 N 行日志 |
| GET | `/api/tasks/{name}/logs/stream` | SSE 流式日志 |
| GET | `/api/tasks/{name}/progress` | 实时/最终进度 |
| DELETE | `/api/tasks/{name}` | 删除任务记录与日志 |
| GET | `/api/config` | 当前生效配置（敏感字段不返回） |

## 关键设计说明

1. **不经过 Kubernetes**：备份由应用直接 exec `rclone copy`，任务名称即进程日志文件名
   （`rclone-full-backup-YYYYMMDD-HHMMSS.log` 等），应用重启后遗留任务自动标记为「中断」。
2. **任务互斥**：同一时间只允许一个备份任务运行；全量触发前自动暂停定时调度，全量结束后自动恢复
   （触发前本就手动暂停则保持暂停）；重叠的定时调度直接跳过（Forbid），`rclone copy` 幂等无损失。
3. **实时进度**：rclone `--use-json-log` 的 stats 行被逐行解析并实时写库——这是对旧版本
   「进度一直为空」问题的根治（旧版依赖轮询 Job Pod 日志）。
4. **SQLite 持久化**：任务元数据与最终进度存于 `DATA_DIR/backup-controller.db`（WAL 模式），
   日志存于 `DATA_DIR/logs/`；支持历史保留数自动清理。调度暂停状态也持久化，重启不丢失。
5. **进度解析容错**：非 stats 行（普通日志、启动信息）自动跳过；`totalBytes=0` 时百分比为 0。
6. **登录认证**：固定账号密码，HMAC 签名 Cookie（HttpOnly、12 小时有效）；`AUTH_SECRET` 留空随机生成，
   重启后需重新登录。
7. **单镜像**：Dockerfile 将 rclone（固定 1.69.0）一并打进运行镜像，一个镜像完成部署与备份执行。
8. **版本锁定**：`rclone/rclone:1.69.0`、`golang:1.22-alpine`、`alpine:3.20`，需同步到内部 Harbor。
