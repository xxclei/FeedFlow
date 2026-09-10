# 10 - Docker 部署（Compose 一键全链路 + CI）【全量版】

前置：阶段 9（功能已齐全）。本阶段做四件事：**多阶段 Dockerfile**（API/Worker 一个文件两个 target）、**docker-compose.yml 六服务全链路**（healthcheck + 条件依赖编排）、**start.sh 本地开发编排**、**GitHub Actions CI**。前九个阶段的代码一行不改，只做"打包与自动化"。

> 原项目一个值得先注意的组织方式：`docker-compose.yml` 放**仓库根**，构建上下文 = 仓库根，两个 Dockerfile 分别在 `backend/`、`frontend/` —— 这决定了 Dockerfile 里 `COPY backend/go.mod` 要带目录前缀、`.dockerignore` 要放根目录一份、`backend/` 一份。

## 目标

- `docker compose up -d --build` 拉起 6 个容器：mysql / redis / rabbitmq / backend(API) / worker / frontend，全部 healthy/running
- backend 与 worker 是**同一个镜像仓库的两个 target**（deps/source 层共享，编译一次）
- 密钥全部走 `.env`（从 `.env.example` 复制），compose `${VAR:-默认值}` 插值 + 应用层环境变量覆盖
- 上传文件落在 named volume，容器随便删数据不丢
- push 到 GitHub → CI 自动 `go vet` + `go test -race`（后端）、`npm ci` + `npm run build`（前端）

## 要写的东西（7 件）

### 1. `backend/Dockerfile`：多阶段构建（逐行讲）

```dockerfile
# syntax=docker/dockerfile:1.7          # ① 声明 BuildKit 语法，--mount=type=cache 才可用

ARG GO_VERSION=1.24.5                   # ② 全局 ARG：换 Go 版本只改一处
ARG GOPROXY=https://goproxy.cn,direct   # ③ 国内代理，CI/容器内下载依赖不再超时
ARG GOSUMDB=sum.golang.google.cn

FROM golang:${GO_VERSION} AS deps       # ④ 阶段1：完整 golang 镜像只干"下依赖"这一件事
ARG GOPROXY                             # ⑤ ARG 不会自动跨 stage，进了 stage 要重新声明才能引用
ARG GOSUMDB
ENV GOPROXY=${GOPROXY} \
    GOSUMDB=${GOSUMDB}
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./   # ⑥ 只拷依赖清单 → 清单不变这层永远命中缓存
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download                     # ⑦ 缓存挂载：模块缓存/编译缓存持久在 builder，不进镜像层

FROM deps AS source                     # ⑧ 阶段2：继承 deps（缓存已热），拷业务源码
COPY backend/ ./
ENV CGO_ENABLED=0                       # ⑨ 静态编译，alpine 里没有 glibc 也能跑

FROM source AS api-build                # ⑩ 阶段3/4：两个构建并行跑，各出自己的二进制
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/api ./cmd

FROM source AS worker-build
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

FROM alpine:3.21 AS base                # ⑪ 阶段5：精简运行时底座，api/worker 公共
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -s /sbin/nologin app
WORKDIR /app
COPY --from=source /src/backend/configs ./configs   # ⑫ 配置文件从 source 阶段拷（不用编译产物）
RUN mkdir -p ./.run/uploads && chown -R app:app /app # ⑬ 上传目录先建好并交权给 app 用户
USER app                                # ⑭ 非 root 运行

FROM base AS api
COPY --from=api-build /out/api /app/api
EXPOSE 8080
ENTRYPOINT ["/app/api"]

FROM base AS worker                     # ⑮ 阶段6/7：两个最终 target，各自只带自己的二进制
COPY --from=worker-build /out/worker /app/worker
ENTRYPOINT ["/app/worker"]
```

逐组讲清楚：

| 组 | 行 | 为什么 |
|---|---|---|
| 语法/ARG | ①②③ | `# syntax=` 启用 BuildKit 前端（`--mount=type=cache` 的前提）；`GOPROXY/GOSUMDB` 让容器内 `go mod download` 在国内网络可用 |
| deps | ④⑤⑥⑦ | 依赖清单与源码**分层拷贝**是缓存优化的根：改业务代码时 `go mod download` 层不重跑；cache mount 比层缓存更进一步——缓存存在 builder 里且跨次构建复用，还不撑大镜像 |
| source | ⑧⑨ | `FROM deps AS source` 继承依赖缓存；`CGO_ENABLED=0` 产纯静态二进制（配合 scratch/alpine） |
| 双构建 | ⑩ | 两个 build 阶段从同一个 source 出发，BuildKit 会**并行**编译；`-trimpath` 去掉机器路径、`-ldflags="-s -w"` 去符号表——二进制小 30% 左右 |
| base | ⑪-⑭ | alpine 约 5MB 的底座；`ca-certificates`（出站 HTTPS）、`tzdata`（时间格式化）必装；`adduser -D -H -s /sbin/nologin app` 建无登录 shell 的普通用户；`chown .run/uploads` 必须在 `USER app` **之前**做 |
| 双 target | ⑮ | `docker build --target api` / `--target worker` 各取所需；compose 里两个服务指向同一个 Dockerfile 的不同 target |

单独手工构建验证：

```bash
docker build -f backend/Dockerfile -t feedsystem-backend:api --target api .
docker build -f backend/Dockerfile -t feedsystem-backend:worker --target worker .
```

### 2. `frontend/Dockerfile` + `nginx.conf`

```dockerfile
FROM node:24-alpine AS build
WORKDIR /src
COPY frontend/package.json frontend/package-lock.json ./   # 依赖分层，同⑥
RUN npm ci                                                  # ci 严格按 lock 安装，可复现
COPY frontend/ ./
RUN npm run build                                           # 产出 /src/dist

FROM nginx:1.27-alpine
COPY frontend/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /src/dist /usr/share/nginx/html
EXPOSE 80
CMD ["nginx", "-g", "daemon off;"]
```

`nginx.conf` 的三段职责（这是容器网络互访的主战场）：

```nginx
server {
  listen 80;
  client_max_body_size 300m;              # 视频上传必须放大默认 1m 限制
  root  /usr/share/nginx/html;
  index index.html;

  location / { try_files $uri $uri/ /index.html; }   # Vue Router history 模式回退

  location /api/ {                        # 反代后端，剥掉 /api 前缀
    proxy_pass http://backend:8080/;      # ★ 服务名当域名用——Compose 网络内置 DNS
    proxy_http_version 1.1;
    proxy_set_header Host $http_host;     # 保留原始 host:port，后端拼绝对 URL 才正确
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;   # 后端判断 http/https 拼上传文件的绝对链接
  }

  location /static/ {                     # 上传文件经后端静态路由吐出
    proxy_pass http://backend:8080/static/;
    proxy_http_version 1.1;
    proxy_set_header Host $http_host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;
  }
}
```

### 3. `.dockerignore`（两处，各司其职）

仓库根（构建上下文是根，这份是主力）：

```
.git
**/node_modules
**/dist
**/.run
**/*.log
npm-debug.log*
```

`backend/`（backend 自己将来单独作上下文构建时兜底）：

```
.git
.run
bin
**/*.log
```

不改的话：构建上下文几个 GB（node_modules + .git 全部上传给 docker daemon），且源码一变缓存全失效。

### 4. `docker-compose.yml`（6 服务全链路）

现代 compose 文件**没有 `version:` 键**，顶层就是 `services:` + `volumes:`。骨架（注释即讲解）：

```yaml
services:
  mysql:
    image: mysql:8.0
    restart: always
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD:-123456}   # 读 .env，缺省 123456
      MYSQL_DATABASE: ${MYSQL_DATABASE:-feedsystem}
      TZ: "Asia/Shanghai"                                   # MySQL 时区，见难点3
    ports: ["3307:3306"]            # 宿主机 3307 防冲突；容器之间仍走 3306
    volumes: [mysql_data:/var/lib/mysql]
    command:
      - --default-authentication-plugin=mysql_native_password
      - --character-set-server=utf8mb4
      - --collation-server=utf8mb4_0900_ai_ci
    healthcheck:
      test: ["CMD-SHELL", "mysqladmin ping -h 127.0.0.1 -uroot -p$${MYSQL_ROOT_PASSWORD} --silent"]
      interval: 5s
      timeout: 5s
      retries: 20                   # MySQL 首次初始化慢，多给几次

  redis:
    image: redis:7-alpine
    restart: always
    environment:
      REDIS_PASSWORD: ${REDIS_PASSWORD:-123456}
    command: ["redis-server", "--appendonly", "yes", "--requirepass", "${REDIS_PASSWORD:-123456}"]
    ports: ["6379:6379"]
    volumes: [redis_data:/data]
    healthcheck:
      test: ["CMD-SHELL", "redis-cli -a \"$${REDIS_PASSWORD}\" ping"]
      interval: 5s
      timeout: 3s
      retries: 20

  rabbitmq:
    image: rabbitmq:3-management    # 带 15672 管理台
    restart: always
    ports: ["5672:5672", "15672:15672"]
    environment:
      RABBITMQ_DEFAULT_USER: ${RABBITMQ_USER:-admin}
      RABBITMQ_DEFAULT_PASS: ${RABBITMQ_PASS:-password123}
    volumes: [rabbitmq_data:/var/lib/rabbitmq]
    healthcheck:
      test: ["CMD-SHELL", "rabbitmq-diagnostics -q ping"]
      interval: 5s
      timeout: 5s
      retries: 20

  backend:
    build:
      context: .                    # 仓库根作上下文 → 所以 Dockerfile 里 COPY backend/...
      dockerfile: backend/Dockerfile
      target: api                   # ← 与 worker 唯一区别
    restart: always
    environment:
      CONFIG_PATH: /app/configs/config.yaml
      JWT_SECRET: ${JWT_SECRET:-feedsystem-dev-secret-key}
      MYSQL_DATABASE: ${MYSQL_DATABASE:-feedsystem}
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD:-123456}
      REDIS_PASSWORD: ${REDIS_PASSWORD:-123456}
      RABBITMQ_USER: ${RABBITMQ_USER:-admin}
      RABBITMQ_PASS: ${RABBITMQ_PASS:-password123}
    ports: ["8080:8080"]
    volumes:
      - ./backend/configs/config.docker.yaml:/app/configs/config.yaml:ro  # 只读覆盖容器内配置
      - backend_uploads:/app/.run/uploads                                 # 上传文件持久化
    depends_on:
      mysql:    { condition: service_healthy }   # 不是"启动了"，是"能接受连接了"
      redis:    { condition: service_healthy }
      rabbitmq: { condition: service_healthy }
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://127.0.0.1:8080/healthz || exit 1"]
      interval: 10s
      timeout: 5s
      retries: 3

  worker:
    build:
      context: .
      dockerfile: backend/Dockerfile
      target: worker                # ← 同一 Dockerfile 另一个 target
    restart: always
    environment: { 与 backend 完全相同的 7 个环境变量 }
    volumes:
      - ./backend/configs/config.docker.yaml:/app/configs/config.yaml:ro
    depends_on: { mysql/redis/rabbitmq 同 backend }
    healthcheck:
      test: ["CMD-SHELL", "pgrep worker || exit 1"]   # worker 没有端口，探进程存活
      interval: 15s
      timeout: 5s
      retries: 3

  frontend:
    build:
      context: .
      dockerfile: frontend/Dockerfile
    restart: always
    ports: ["5173:80"]              # 宿主机 5173 对齐 Vite 习惯端口，容器里是 nginx 的 80
    depends_on:
      - backend                     # 简单形态：只保证启动顺序，不要求 healthy
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://127.0.0.1:80/ || exit 1"]
      interval: 10s
      timeout: 5s
      retries: 3

volumes:
  mysql_data:
  redis_data:
  rabbitmq_data:
  backend_uploads:
```

**`$$` 转义（compose 高频坑）**：healthcheck 的 `test` 里是 `$${MYSQL_ROOT_PASSWORD}`（两个 `$`），compose 解析时把 `$$` 折叠成字面 `$`，真正到容器 shell 里展开的是**容器自己的环境变量**。对比 redis 的 `command` 里是单 `${REDIS_PASSWORD:-123456}`——那是 compose 在宿主机解析时就用 `.env` 替换成明文了。一个变量、两种展开时机，务必分清。

**环境变量注入与 config.docker.yaml 的对应关系**（分两层，各管一摊）：

| 注入的环境变量 | 应用层谁消费 | 覆盖 config 的哪个字段 |
|---|---|---|
| `CONFIG_PATH` | `cmd/main.go` / `cmd/worker/main.go` 里 `os.Getenv` | 不覆盖，只指定"读哪个配置文件" |
| `JWT_SECRET` | `internal/auth/jwt.go` 里 `os.Getenv`（config 结构体里根本没有 secret 字段） | 独立于配置文件 |
| `MYSQL_ROOT_PASSWORD` | `ApplyEnvOverrides` | `database.password` |
| `MYSQL_DATABASE` | 同上 | `database.dbname` |
| `REDIS_PASSWORD` | 同上 | `redis.password` |
| `RABBITMQ_USER` / `RABBITMQ_PASS` | 同上 | `rabbitmq.username` / `rabbitmq.password` |

而 **host 一个都没注入**：`config.docker.yaml` 里 `database.host: mysql`、`redis.host: redis`、`rabbitmq.host: rabbitmq`，靠挂载文件解决。原项目的取舍是——**host 是部署拓扑（容器内外不同），用配置文件区分；密码是敏感值（不进 git），用环境变量注入**。覆盖链完整顺序：配置文件 → `ApplyEnvOverrides` 环境变量覆盖 → 仍为空时用 `LoadLocalDev` 的内置默认值。另外两个进程入口都有 `godotenv.Load()`（找不到 `.env` 只打日志继续），这是给"本机直接 go run"场景读密钥用的，容器里读不到也无害。

`config.docker.yaml` 全文（对照你的 `configs/config.yaml`，只有 host 不同）：

```yaml
server:
  port: 8080
database:
  host: mysql            # ← 服务名即 DNS
  port: 3306             # ← 容器间直连端口，不是 3307
  user: root
  password: 123456       # 会被 MYSQL_ROOT_PASSWORD 环境变量覆盖
  dbname: feedsystem
redis:
  host: redis
  port: 6379
  password: 123456
  db: 0
rabbitmq:
  host: rabbitmq
  port: 5672
  username: admin
  password: password123
observability:
  pprof:
    enabled: false       # 容器里 6060 没映射出去，关掉
    api_addr: localhost:6060
    worker_addr: localhost:6061
```

### 5. `.env.example`（密钥模板）

```bash
# MySQL
MYSQL_ROOT_PASSWORD=123456
MYSQL_DATABASE=feedsystem
# Redis
REDIS_PASSWORD=123456
# RabbitMQ
RABBITMQ_USER=admin
RABBITMQ_PASS=password123
# JWT (生产环境务必修改为随机强密钥)
JWT_SECRET=feedsystem-dev-secret-key
```

`cp .env.example .env` 后 compose 自动读取（无需 export）；确认 `.gitignore` 里有 `.env`（原项目有）。这就是 compose 里每个 `${VAR:-默认值}` 的数据源。

### 6. `start.sh`（本地开发编排，约 230 行）

定位和 compose 互补：compose 是"全部容器化"，start.sh 是**混合模式**——依赖（MySQL/Redis/RabbitMQ）在容器里，API/Worker/前端用 `go run`/`npm run` 跑在本机，改代码即时生效。复刻这些骨架：

```bash
#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"; cd "$ROOT_DIR"

# ── 环境变量开关（全部有默认值）──────────────────────────
# START_REDIS / START_RABBITMQ / START_BACKEND / START_WORKER / START_FRONTEND = 1
# STOP_DOCKER=0        # 1 = 退出时把脚本拉起的 compose 服务一并 stop
# COMPOSE_FILE         # 默认 ./docker-compose.yml
# FRONTEND_INSTALL=auto  # auto=缺 node_modules 才 npm install；1/0 强制开关
# FRONTEND_SCRIPT=dev    # dev|preview|build
# CONFIG_PATH          # 没显式给、且脚本自己要起 rabbitmq 时 → configs/config.compose-local.yaml
if [ -z "${CONFIG_PATH:-}" ] && [ "$START_RABBITMQ" = "1" ]; then
  CONFIG_PATH="configs/config.compose-local.yaml"; fi   # ★ 宿主机进程要连 127.0.0.1:3307
export CONFIG_PATH

trap cleanup INT TERM EXIT        # Ctrl+C / kill 都走 cleanup
cleanup() {                       # 逆序杀：frontend → worker → backend
  kill "$FRONTEND_PID" "$WORKER_PID" "$BACKEND_PID" 2>/dev/null || true
  # Windows Git Bash 子进程收信号不可靠 → taskkill //PID x //T //F 兜底
  if [ "$STOP_DOCKER" = "1" ]; then $COMPOSE_CMD -f "$COMPOSE_FILE" stop; fi
}

detect_compose() {                # docker compose 优先，退回 docker-compose 老命令
  docker compose version >/dev/null 2>&1 && COMPOSE_CMD="docker compose" && return 0
  command -v docker-compose >/dev/null 2>&1 && COMPOSE_CMD="docker-compose"
}

start_rabbitmq_compose() {        # compose up -d rabbitmq → docker exec 探活 30 次×1s
  $COMPOSE_CMD -f "$COMPOSE_FILE" up -d rabbitmq
  rabbit_cid="$($COMPOSE_CMD ps -q rabbitmq)"
  for i in $(seq 30); do
    docker exec "$rabbit_cid" rabbitmq-diagnostics -q ping && return 0; sleep 1; done
}

start_redis() {                   # 本机 redis-cli ping 通 → 直接复用不重启
  redis-cli -h $REDIS_HOST -p $REDIS_PORT ping && return 0
  nohup redis-server --bind $REDIS_HOST --port $REDIS_PORT >"$RUN_DIR/redis.log" 2>&1 &
}

start_backend_bg()  { (cd "$BACKEND_DIR"  && go run ./cmd) &        BACKEND_PID=$!;  echo $BACKEND_PID  >"$RUN_DIR/backend.pid"; }
start_worker_bg()   { (cd "$BACKEND_DIR"  && go run ./cmd/worker) & WORKER_PID=$!;  echo $WORKER_PID   >"$RUN_DIR/worker.pid"; }
start_frontend_bg() { [ 需要时 npm install ]; (cd "$FRONTEND_DIR" && npm run $FRONTEND_SCRIPT) & FRONTEND_PID=$!; }

# 按依赖顺序启动：rabbitmq(条件) → redis(条件) → backend → worker → frontend → wait
```

功能清单（验收时逐条对）：`require_dir`/`require_cmd` 前置检查；PID 写 `.run/*.pid`；redis 日志进 `.run/redis.log`；`wait` 挂住主进程；Windows 兼容（taskkill）。注意它和难点的呼应：**宿主机进程连依赖要走映射端口 3307**，所以脚本自动切到 `config.compose-local.yaml`（host=localhost、port=3307、pprof 开）。

### 7. CI：`.github/workflows/ci.yml`（完整流水线，本阶段亮点）

```yaml
name: CI

on:
  pull_request:              # 所有 PR 都跑
  push:
    branches: [main, master] # 直推主干也跑

permissions:
  contents: read             # 最小权限：只读代码，不碰任何写接口

concurrency:
  group: ci-${{ github.workflow }}-${{ github.ref }}   # 按分支分组
  cancel-in-progress: true   # 同分支连推 3 个 commit → 只跑最后一个，省 Actions 分钟数

jobs:
  backend:
    name: Backend
    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: backend       # ★ monorepo 必配，否则每条命令都要 cd
    steps:
      - name: Checkout
        uses: actions/checkout@v4
      - name: Setup Go
        uses: actions/setup-go@v5
        with:
          go-version: "1.24.x"
          cache-dependency-path: backend/go.sum   # 告诉缓存 action 去 monorepo 子目录找依赖清单
      - name: Download dependencies
        run: go mod download
      - name: Vet
        run: go vet ./...
      - name: Test
        run: go test -race -count=1 ./...        # -race 查数据竞争；-count=1 禁用测试结果缓存
  frontend:
    name: Frontend
    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: frontend
    steps:
      - name: Checkout
        uses: actions/checkout@v4
      - name: Setup Node
        uses: actions/setup-node@v4
        with:
          node-version: "22"
          cache: npm                             # 缓存 ~/.npm，命中后 npm ci 提速明显
          cache-dependency-path: frontend/package-lock.json
      - name: Install dependencies
        run: npm ci
      - name: Build
        run: npm run build                       # 编译都过 = TypeScript 类型检查过了一道
```

两个 job **并行**跑、互不依赖。设计取舍：`go vet` + `-race` 测试是性价比最高的质量闸门，不堆 lint 全家桶；前端只验"能构建"。你现在就可以把这个文件放进仓库——`go test` 对没有测试文件的包也能通过，等阶段里写了 `_test.go` 它自动开始干活。

## 关键设计决策

| 问题 | 原项目答案 |
|---|---|
| API 和 Worker 打一个镜像还是两个？ | 一个 Dockerfile、两个 target（deps/source/构建层全部共享，磁盘省一半；部署时两个服务可独立重启/扩容） |
| Dockerfile 放哪、上下文多大？ | `backend/Dockerfile`、`frontend/Dockerfile`，但 context 都用仓库根——这样 backend/、frontend/、.github/ 等相对路径统一，代价是要靠 `.dockerignore` 控制上下文体积 |
| 配置怎么进容器？ | 三层机制：挂载 `config.docker.yaml` 只读覆盖 `/app/configs/config.yaml`（拓扑差异）→ `environment` 注入密钥类变量（`ApplyEnvOverrides` 覆盖 + `JWT_SECRET` 直读）→ 都没有则用内置默认值 |
| 依赖未就绪怎么办？ | 三层保险：healthcheck（探真实可用性）→ `depends_on: condition: service_healthy`（编排等它）→ 应用自身降级/重试（阶段 0/7/9 的启动降级与 Worker 重连，兜最后） |
| 上传文件持久化 | named volume `backend_uploads:/app/.run/uploads`（配合 `r.Static("/static", ...)`），容器可丢弃、数据不丢；镜像里已 `chown app:app` 保证非 root 可写 |
| MySQL 端口 | 宿主机映射 3307 防冲突；容器之间一律 3306 + 服务名 |
| worker 挂了怎么自愈？ | 双保险：healthcheck 用 `pgrep worker` 探进程 + `restart: always`；进程内每个 Worker 的 channel 断开后由重连循环 5 秒重试恢复（`runWorkerWithRetry`），不靠杀容器 |
| frontend 为什么不用 `condition: service_healthy`？ | nginx 不需要在启动时连上 backend——请求时代理失败返回 502，backend 一 healthy 就自愈，简单 `depends_on` 只排启动顺序即可 |
| 密钥为什么默认值到处都是？ | 开发体验优先：没有 `.env` 也能一键起；生产用 `.env` 覆盖。模板进 git、真值不进 |

## 难点清单（本阶段真正难的地方）

1. **容器网络互访（服务名 vs 127.0.0.1）**：compose 默认建一张网络，服务名是内置 DNS 记录。容器内 `localhost:3306` 是**容器自己**，连 MySQL 必写 `mysql:3306`；`127.0.0.1:8080` 的 healthcheck 却恰恰**应该**探容器自己。宿主机访问走 `ports` 映射（3307/5173），容器互访走服务名 + 容器端口，两套端口体系不能混。推演三遍：config.docker.yaml（服务名）↔ nginx `proxy_pass http://backend:8080`（服务名）↔ start.sh 混合模式连 `127.0.0.1:3307`（宿主机视角）。
2. **构建缓存优化**：三个杠杆缺一不可——(a) 依赖清单先拷、源码后拷（层缓存）；(b) BuildKit `--mount=type=cache` 把 `/go/pkg/mod`、`/root/.cache/go-build` 挂成持久缓存（源码变了依赖也不重下，且缓存不进镜像层）；(c) `.dockerignore` 砍掉无关文件（上下文小 → COPY 层失效概率低）。验证方法：改一行 Go 代码重新 build，`go mod download` 层必须是 CACHED。
3. **时区处理**：链路上有四处各管一段——MySQL 容器 `TZ=Asia/Shanghai`（NOW()/日志用）；alpine 装 `tzdata`（Go 格式化时间的前提，不装则 `Asia/Shanghai` 加载失败回落 UTC）；DSN `loc=Local`（驱动按 Go 进程的本地时区解析时间）；backend/worker 容器**没设 TZ → 进程时区是 UTC**。写读往返（gorm 写时间 → MySQL DATETIME → 读回）在 UTC 里自洽，但与 MySQL 端 `NOW()` 比较、直接看库里的时间会差 8 小时——进阶改进：给 backend/worker 也加 `TZ: "Asia/Shanghai"`，或在 DSN 显式 `loc=Asia%2FShanghai`。
4. **healthcheck 的两套变量展开**：`command` 里的 `${VAR}` 由 compose 在宿主机用 `.env` 展开；healthcheck `test` 里的 `$${VAR}` 折叠成 `$VAR` 后由**容器 shell** 展开。写错的表现：redis healthcheck 拿空密码 ping → 永远 unhealthy → `depends_on` 全体卡住。
5. **非 root 运行的文件权限**：`adduser` → `mkdir/chown` → `USER app` 的顺序不能乱；挂载 named volume 后 volume 首次初始化会继承镜像内目录属主，所以镜像里那步 `chown -R app:app /app` 不是可有可无。

## 亮点（原项目在部署层的工程亮点）

- **一个 Dockerfile 出两个运行镜像**：deps/source 层共享、双构建并行、base 底座共享——golang 编译镜像 800MB+ 级，最终运行镜像只有几十 MB；api/worker 又能独立重启扩容
- **BuildKit cache mount**：比"依赖分层"更强的第二层缓存，Go 模块与编译缓存在多次构建间持久复用，且不污染镜像层
- **healthcheck 编排**：`condition: service_healthy` 把"容器启动"和"服务可用"区分开，启动竞态在编排层就被消灭；backend 探 `/healthz`（应用真实状态）、worker 探 `pgrep`（无端口进程）、frontend 探首页，探法各得其所
- **CI 自动化**：PR + push 双触发、按 ref 并发去重取消、最小 `permissions`、monorepo 精准缓存（`cache-dependency-path`）、`-race` 数据竞争检测——30 行 YAML 拿到一套合格的回归闸门
- **配置的三层覆盖 + 密钥外置**：文件管拓扑、环境变量管密钥、默认值保底兜底；`.env.example` 进 git、`.env` 进 gitignore
- **非 root + 精简底座**：alpine + `nologin` 用户 + `-trimpath -ldflags="-s -w"`，安全面和体积一起收
- **start.sh 的跨平台细节**：`docker compose`/`docker-compose` 双探测、Git Bash 下 taskkill 兜底、Redis 探活复用、退出时 `STOP_DOCKER=1` 逆序清理

## 常见的坑

- Dockerfile 里 `COPY backend/go.mod backend/go.sum` 忘了先单独拷 → 改一行业务代码就重下全部依赖
- 忘了 `# syntax=docker/dockerfile:1.7` → `--mount=type=cache` 直接报错"不支持的 flag"
- ARG 作用域：`FROM` 之前声明的 ARG，进 stage 后要**重新 `ARG GOPROXY` 声明**才可引用
- alpine 里没 tzdata → 时间全 UTC；MySQL 容器设了 `TZ` 而 backend 容器没设 → 库里时间与 `NOW()` 差 8 小时（见难点 3）
- 容器内访问 `localhost:3306` 是容器自己 → host 必须写服务名；反过来 healthcheck 探 `127.0.0.1` 是对的（探自己）
- healthcheck 里 `$${VAR}` 写成 `${VAR}` → compose 展开成 `.env` 值/空串，容器内语义变了
- `depends_on` 不写 `condition` 只是启动顺序，不保证服务可用；MySQL 首次初始化要几十秒，retries 给足（20）
- 构建上下文含 `.git`/`node_modules` → `.dockerignore` 必写，否则上下文上传慢且缓存失效
- `npm ci` 要求 `package-lock.json` 已提交；只提交 `package.json` 会直接失败（这是特性不是 bug）
- nginx 不放 `client_max_body_size` → 视频上传 413；`proxy_set_header Host $http_host` 漏掉 → 后端拼出的上传文件绝对 URL 域名不对
- worker 的自愈理解偏差：原项目不是"断连就退出等容器重启"，而是进程内重连循环（每个 Worker 独立 channel，5 秒重试）；`restart: always` + pgrep healthcheck 是进程级兜底
- 配置路径是相对**进程工作目录**的：容器里 `WORKDIR /app` + `CONFIG_PATH=/app/configs/config.yaml`，二者要能对上
- CI 里忘配 `working-directory`/`cache-dependency-path` → 在仓库根找 `go.mod` 报错、缓存永远不命中

## 验收清单

- [ ] `docker compose up -d --build` 一次成功，`docker compose ps` 6 容器全 healthy/running
- [ ] 浏览器走通：`http://localhost:5173` 注册→登录→上传→发布→点赞（MQ 链路在容器间也工作）；`http://localhost:8080/healthz` 返回 ok
- [ ] RabbitMQ 管理台 `http://localhost:15672`（admin/password123）能看到 4 个 exchange/queue；`docker compose logs worker` 有 4 个 consumer 启动日志
- [ ] `cp .env.example .env` 改掉 `JWT_SECRET` → `docker compose up -d` 后新登录的 token 前后行为符合预期（验证环境变量注入链）
- [ ] `docker compose stop redis && sleep 5 && docker compose start redis` → 业务不中断（阶段 7 降级 + 自愈在容器环境复验）
- [ ] 修改一行 Go 代码重新 build → 秒级完成，`go mod download` 层 CACHED（构建缓存验收）
- [ ] `docker compose down -v` 再起 → 上传的视频经 `backend_uploads` volume 仍在（先不 -v 验证；-v 会清数据，用于演练后清理）
- [ ] push 到 GitHub → Actions 两个 job 全绿；本地故意写一个 vet 错误 → CI 红灯
- [ ] `START_FRONTEND=0 ./start.sh` 混合模式可用，退出 Ctrl+C 无残留进程（Windows 下确认 taskkill 生效）

## 后续阶段回填清单（到时回来改，现在不做）

- **阶段 7（Redis 缓存 + 限流）**：限流阈值若环境变量化（如 `RATELIMIT_LOGIN_PER_MIN`、`RATELIMIT_REGISTER_PER_HOUR`），要同步补三处：compose 的 backend/worker `environment` → `.env.example` → 应用侧 `ApplyEnvOverrides` 或专用 `os.Getenv`；`REDIS_PASSWORD` 已注入无需再动；验证限流 key 前缀（原项目 `feedsystem:ratelimit:<动作>:<主体>`，你的是 `myfeed:...`）在容器内 redis-cli 可见
- **阶段 8/9 已就绪**：热榜与 MQ 不引入新环境变量/新服务，容器链路无需改
- **阶段 11（前端）**：生产走 nginx `/api/` 反代（本阶段已配好），`vite.config.ts` 的代理只服务开发期；若前端有新的构建期环境变量（`import.meta.env.*`），要补进 `frontend/Dockerfile` 的 build 阶段 `ARG/ENV`
- **阶段 12（SSE/通知，规划中）**：SSEHub 在 API 进程内（无需新容器/新端口），但要给 `nginx.conf` 回填 `/api/notification/stream` 专属 location：`proxy_buffering off`（否则 nginx 攒缓冲，事件迟迟不到浏览器）、`proxy_read_timeout 3600s`（长连接不被 60s 默认值掐断）、`proxy_http_version 1.1` + `Connection ""`；复验 EventSource 的 `?token=` 经反代后鉴权仍通过
- **通用原则**：以后每引入一个环境变量，检查四件套是否同步——`.env.example` → compose `environment` → 应用读取处 → 本文档的对应关系表

## 原项目对照

- `docker-compose.yml`（6 服务完整版，本篇骨架的原版）
- `backend/Dockerfile`（多阶段 7 target 原版，含注释）
- `frontend/Dockerfile` + `frontend/nginx.conf`（node 构建 + nginx 反代）
- `.dockerignore`（仓库根）与 `backend/.dockerignore`（backend 目录）
- `.env.example`、`.gitignore`（确认 `.env` 被忽略）
- `start.sh`（混合模式编排原版，约 230 行）
- `backend/configs/config.docker.yaml` / `config.compose-local.yaml` / `config.yaml`（三套配置对照）
- `backend/internal/config/loadconfig.go` 的 `ApplyEnvOverrides` / `LoadLocalDev`（环境变量覆盖链）
- `backend/cmd/main.go` 与 `backend/cmd/worker/main.go` 开头（`godotenv.Load` + `CONFIG_PATH` 解析）
- `backend/internal/http/router.go:27`（`/healthz`）
- `backend/cmd/worker/main.go` 的 `runWorkerWithRetry`（进程内重连自愈）
- `.github/workflows/ci.yml`（CI 原版）
