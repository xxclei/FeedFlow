# 10 - Docker 部署（Compose 一键全链路）

前置：阶段 9（功能已齐全）。

## 目标

`docker compose up -d --build` 拉起 6 个容器：mysql / redis / rabbitmq / backend(API) / worker / frontend；API 与 Worker **同一个镜像的两个 target**。

## 要写的三样东西

### 1. 多阶段 Dockerfile（`deploy/backend.Dockerfile` 或按原项目放 backend/ 下）

```dockerfile
# 阶段1 build：完整 golang 镜像编译两个二进制
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download                      # 先 COPY 依赖文件 → 利用层缓存
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd
RUN go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# 阶段2 base：alpine 运行底座（公共）
FROM alpine:3.21 AS base
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -s /sbin/nologin app
WORKDIR /app
COPY configs ./configs
USER app

# 阶段3/4：两个 target 各自只拷贝自己的二进制
FROM base AS api;    COPY --from=build /out/api /app/api;     ENTRYPOINT ["/app/api"]
FROM base AS worker; COPY --from=build /out/worker /app/worker; ENTRYPOINT ["/app/worker"]
```

`CGO_ENABLED=0` 静态编译 + alpine 才能跑；`-trimpath -ldflags="-s -w"` 去路径缩体积；非 root 用户运行。

### 2. docker-compose.yml（6 服务）

在阶段 0 的 deps 基础上加三个业务服务，**健康检查 + 条件依赖是灵魂**：

```yaml
  backend:
    build: { context: ., dockerfile: deploy/backend.Dockerfile, target: api }
    ports: ["8080:8080"]
    volumes:
      - ./configs/config.docker.yaml:/app/configs/config.yaml:ro   # 容器内配置覆盖
    depends_on:
      mysql:    { condition: service_healthy }     # 等 MySQL 真正可接受连接
      redis:    { condition: service_healthy }
      rabbitmq: { condition: service_healthy }
  worker:
    build: { ..., target: worker }                 # 同镜像不同 target
    volumes: [ 同上挂配置 ]
    depends_on: [ 同 backend ]
```

healthcheck 写法（mysql 例）：

```yaml
    healthcheck:
      test: ["CMD-SHELL", "mysqladmin ping -h127.0.0.1 -uroot -p123456 --silent"]
      interval: 5s
      retries: 20
```

`config.docker.yaml`：host 全部改成服务名（`mysql` / `redis` / `rabbitmq`）——**Compose 网络里服务名就是 DNS**。

### 3. start.sh（本地开发编排，可选但推荐）

原项目的很完善（330 行），核心功能你复刻这几个就够：
- 环境变量开关：`START_BACKEND / START_WORKER / START_FRONTEND / START_RABBITMQ`（默认 1）
- 起 RabbitMQ（compose）→ 探活 30s；Redis 本机有就跳过
- 后台起 backend/worker/frontend，PID 写入 `.run/*.pid`
- `trap cleanup INT TERM EXIT`：Ctrl+C 时按逆序 kill

## 关键设计决策

| 问题 | 原项目答案 |
|---|---|
| API 和 Worker 打一个镜像还是两个？ | 一个镜像两个 target（编译产物共享层，省空间；部署时可分开扩容） |
| 配置怎么进容器？ | 挂载只读文件覆盖 `/app/configs/config.yaml`（12-factor 应该用环境变量，这是可改进点） |
| 依赖未就绪怎么办？ | 三层保险：healthcheck + `condition: service_healthy` + 应用自身的降级/重试 |
| 上传文件持久化 | named volume `backend_uploads:/app/.run/uploads`（容器可丢弃，数据不丢） |
| MySQL 端口 | 宿主机映射 3307 防冲突，容器间仍走 3306 |

## 常见的坑

- Dockerfile 里 `COPY go.mod go.sum` 忘了先单独拷 → 改一行业务代码就重下全部依赖
- alpine 里没 tzdata → 时间全 UTC，和 DSN `loc=Local` 叠出 8 小时偏差
- 容器内访问 `localhost:3306` 是容器自己 → host 必须写服务名
- worker 没有 healthcheck 也能跑，但它依赖 MQ，MQ 重启时 worker 的 channel 断连 → 靠 `restart: always` + deliveries channel closed 退出重启来恢复（原项目策略，理解它为什么可行）
- 构建上下文含 `.git`/`node_modules` → `.dockerignore` 必写

## 验收清单

- [ ] `docker compose up -d --build` 一次成功，6 容器全 healthy/running
- [ ] 浏览器走通：注册→登录→上传→发布→点赞（MQ 链路在容器间也工作）
- [ ] `docker compose stop redis && sleep 5 && docker compose start redis` → 业务不中断（阶段 7 降级 + 自愈在容器环境复验）
- [ ] `docker compose logs worker` 能看到 4 个 consumer 启动日志
- [ ] 修改一行 Go 代码重新 build → 秒级利用缓存（`go mod download` 层没重跑）

## 原项目对照

- `docker-compose.yml`（完整 6 服务版）
- `backend/Dockerfile`（多阶段四 target 原版）
- `backend/configs/config.docker.yaml`
- `start.sh`（进阶参考）
