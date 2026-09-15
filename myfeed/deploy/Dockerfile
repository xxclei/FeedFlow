# myfeed 后端镜像。**api 和 worker 共用这一个**，只是启动命令不同。
#
# ---------- 为什么共用一个镜像 ----------
#
# 两个进程的可执行文件都来自同一份 go.mod、同一份 internal/，分开写两个 Dockerfile
# 就有两个地方可以各自漂移（一个装了 tzdata 另一个忘了、一个的 ffmpeg 是旧版本）。
# 而它们**必须**同构：worker 要能读懂 api 写的文件、要能连同一个库、
# 要在同一个时区里解释同一批时间戳。
#
# 共用的另一个好处是镜像只有一份：2核2G 的机器上，多存 300 MB 的 ffmpeg
# 副本不是零成本。
#
# ---------- 为什么用 debian 而不是 alpine ----------
#
# alpine 更小，但它的 musl libc 和 ffmpeg 打包方式都需要额外注意
# （alpine 的 ffmpeg 包依赖也偏重，实际省不了多少）。这里选 debian-slim：
# apt 里的 ffmpeg 是 Debian 官方维护的、和镜像里其它库的 libc 一致，
# **少一个"某条命令在别处能跑这里不能"的变量**。
# 这个项目已经因为"平台差异导致的静默失败"吃过太多次（见 PROGRESS 里那几条），
# 不打算在部署这一层再引入一个。

# ========== 构建阶段 ==========
FROM golang:1.26-bookworm AS build

WORKDIR /src

# ---------- 国内模块代理（上云之后才暴露出来的一条）----------
#
# 这个项目一直在 Windows 本机开发，本机装 Go 时大概率配过国内代理
# （或者压根没注意过），所以这条从来没被验证过。服务器上是从零开始
# 的官方镜像 —— go 的默认代理是 proxy.golang.org，而它在大陆
# **基本连不上**（DNS 污染 + 出口限制）。
#
# 表现不是报错，是 `go mod download` **长时间完全无输出**，最后超时。
# 看起来像"卡住了"，实际是每个请求都在等一个不会来的响应。
#
# goproxy.cn 是七牛提供的公共代理，大陆可用。
# `,direct` 是回退链：代理上没有的模块，再按模块原始地址直连一次。
ENV GOPROXY=https://goproxy.cn,direct

# 先只 COPY 依赖清单再 go mod download：这样改一行 Go 代码不会让
# "下载依赖"这一层缓存失效。是这个文件里唯一一处**为了构建速度**的设计。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0：全项目的依赖都是纯 Go（gin / go-sql-driver / go-redis / amqp091），
# 没有一处需要 cgo。静态链接之后产出的是一个不依赖 libc 的二进制 ——
# 运行阶段换成任何基础镜像都能跑，也让"宿主机和容器 glibc 版本不一致"
# 这类问题不存在。
#
# GOOS=linux 在这里是**必须显式写**的：这个项目的开发机是 Windows，
# 而 buildx/普通 build 默认按**构建机**的 GOOS 走。不写的话
# 本机 `docker build` 会产出一个 Windows 的 .exe，然后容器里 exec 报
# "exec format error" —— 报错信息完全指不到正确方向。
#
# -trimpath 去掉产物里的本机绝对路径；-s -w 去掉符号表和调试信息（体积约减 30%）。
ENV CGO_ENABLED=0 GOOS=linux

# 两个入口分别编译，产出两个静态二进制。
RUN go build -trimpath -ldflags="-s -w" -o /out/myfeed-api    ./cmd \
 && go build -trimpath -ldflags="-s -w" -o /out/myfeed-worker ./cmd/worker

# ========== 运行阶段 ==========
FROM debian:bookworm-slim

# 这四个包各自对应一条**不改就静默出错**的路径：
#
#   ffmpeg   转码。不装的话 worker 里 exec 失败 —— 而它不是启动期失败，
#            是**每条视频上传之后**才失败，表现为 transcode_status 恒为 'failed'，
#            前端自动回落直传原片，看起来"能播"。（配置里的 transcode.ffmpeg_path
#            必须和这里的安装位置对上，见 config.prod.example.yaml）
#   tzdata   让 TZ 环境变量真的生效。**不装的话 TZ=Asia/Shanghai 会被忽略**，
#            time.Local 仍是 UTC —— 也就是"设了等于没设"。这条会直接搞坏
#            feed 的排序（见 config.prod.example.yaml 里时区那一段）。
#   curl     compose 里 api 的 healthcheck。debian:bookworm-slim **不带任何
#            HTTP 客户端**（curl 和 wget 都没有），没有它就只能退化成
#            "端口开着就算健康"的 TCP 探测 —— 而"进程活着但 handler 不通"
#            正是本项目反复吃亏的那种失败。顺带在服务器上进容器排查时也能用。
#   ca-certificates  Go 进程自己不发外部 HTTPS 请求，留着是为了将来接
#            对象存储/CDN 时不用回头改镜像。
RUN set -eu; \
    for f in /etc/apt/sources.list /etc/apt/sources.list.d/*.sources /etc/apt/sources.list.d/*.list; do \
      [ -f "$f" ] || continue; \
      sed -i 's|deb\.debian\.org|mirrors.cloud.tencent.com|g' "$f"; \
    done; \
    if ! grep -rq 'mirrors\.cloud\.tencent\.com' /etc/apt/; then \
      echo '>>> 换源没生效：没在任何 apt 源文件里找到 deb.debian.org。'; \
      echo '>>> 执行 ls /etc/apt/sources.list.d/，再 cat 里面那个文件看看实际长什么样。'; \
      exit 1; \
    fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends ffmpeg ca-certificates tzdata curl; \
    rm -rf /var/lib/apt/lists/*

# ---------- 上面这段换源，是实测之后才加的 ----------
#
# 第一次 build 时卡在这一步 21 分钟没出来，量到的持续速率是 **13~15 KB/s**，
# 而 ffmpeg 在 Debian 上要拉 100 多个包（装完 300~400 MB）——
# 按那个速度还要 2~3 小时。
#
# 注意它不是"一直慢"：最开始那个 8.79 MB 的包清单 0.8 秒就下完了（11 MB/s），
# 所以很容易被误判成"网络没问题"。**首包命中 CDN 缓存，不代表持续速率。**
# 这一条值得记住：一个瞬时值不能代表一条链路。
#
# 换到腾讯自家镜像后实测 968 KB/s ~ 1.39 MB/s，同一批包一两分钟就完。
#
# `if ! grep -rq ...` 那一小段是**故意让它炸**的：万一将来换基础镜像、
# apt 源不再是 deb.debian.org 的格式，sed 会静默匹配不到任何东西 ——
# 然后构建就在那个慢源上再爬几个小时，而且**没有任何报错**。
# 宁可在这里明确失败。这类"改了没生效但不报错"正是本项目反复吃亏的地方。
#
# ---------- 这里踩的第二个坑：源文件**在哪**也会变 ----------
#
# 第一版只 sed 了 /etc/apt/sources.list，结果：
#     sed: can't read /etc/apt/sources.list: No such file or directory
#
# Debian 12 起 apt 的源格式在往 **deb822**（一段一段的 key: value，而不是
# 一行一个 `deb http://...`）迁移，新镜像里根本没有 sources.list 这个文件，
# 内容在 /etc/apt/sources.list.d/debian.sources。
# 所以这里改成**扫两个位置**（老式 .list + 新式 .sources），不再写死路径。
#
# 注意这不是"上次那个坑没修好"，是同一个教训的第二种表现：
#     上次是「匹配到了但没生效」，这次是「路径本身变了」。
# 两次都栽在"我假设了某个东西长什么样"。凡是这种假设，都得配一个会响的检查。

# ---------- WORKDIR 是**必须**的，不是习惯 ----------
#
# cmd/main.go 和 cmd/worker/main.go 里都是 `config.Load("configs/config.yaml")`，
# 这是一个**相对于进程工作目录**的路径。没有 WORKDIR 的话 CWD 是 `/`，
# 那个文件不存在 → `log.Fatalf("加载配置失败")` → 容器反复重启。
#
# 两个进程的 WORKDIR 必须**完全相同**：它们除了配置，还要从同一个
# uploads 根目录读写文件，而路径解析的基准就是这里。
WORKDIR /app

COPY --from=build /out/myfeed-api    /app/myfeed-api
COPY --from=build /out/myfeed-worker /app/myfeed-worker

# 刻意**不 COPY configs/**：那份配置里有数据库密码，而镜像是会被
# 推送、导出、分享的东西。配置在运行时用 volume 挂到
# /app/configs/config.yaml（路径必须和代码里那个字面量一致），见 compose 文件。
#
# 默认启动 api；worker 服务在 compose 里覆盖 command。
ENV TZ=Asia/Shanghai
EXPOSE 8080
CMD ["/app/myfeed-api"]
