# 转码 / 多档 HLS / 过载降级 实测记录（扩展 B~E）

> 这是[「视频播放体验」扩展](../)的记录文件，和 `bench-notes-qoe.md`（阶段 A）配套。
> 两个文件都指向同一个闭环，但测的是**不同的东西**，所以分开：
>
> ```
> A  上传门禁探测 → B 超阈值自动转码多档 → C 播放端 ABR → D 过载时服务端只发低档 → E
>    └ bench-notes-qoe.md ──────────────┘  └── bench-notes-transcode.md ──┘
> ```
>
> ⚠ **A~E 的字母编号和这个文件里的阶段编号不是一套**：本文件按计划里的
> 「阶段 B（门禁）/ C（转码 worker + HLS）/ D（播放端）/ E（降级）」来分节。

---

## 一、为什么做这个扩展

2026-09-15 测完视频分发（`bench-notes-video-delivery.md`），结论是瓶颈在
12 Mbps 上行、服务端差了 960 倍。但那次测量暴露了一个**结构性问题**：

> "能撑 33 人"是**假的安全感** —— 不是架构挣来的，是上传者**碰巧**传了
> 0.36 Mbps 的素材。项目**没有转码**，码率 100% 由上传者决定。只要有人传
> 手机原片（1080p / 10~15 Mbps），边界立刻塌到"不到 1 人"。

**这次普查把那个"碰巧"量化了**（见 §三.3）：存量 135 个素材里
**128 个是 1920×1080** —— 分辨率是最高档，码率却只有 0.16~1.16 Mbps。

也就是说：播放器**报告**的是 1080p、用户**看到**的是 1080p，
而实际带宽开销只有 1080p 档位目标的 **1/12**。这就是"假的安全感"的精确形态 ——
它不是"素材小"，是**分辨率和码率脱钩**，而脱钩的原因是没有转码统一口径。

目标：把码率决定权从上传者手里收回服务端，并让服务端过载时能主动降级。

---

## 二、前置：ffmpeg 的安装方式（已确认的决策）

| 决策 | 选定 | 理由 |
|---|---|---|
| 安装位置 | **手动解压到 `myfeed/.run/bin/`** | 不动系统 PATH：全局安装会污染开发机，且版本随人不同，"在我机器上转码参数正常"这类问题不该出现在复刻项目里 |
| 存量 111/135 条 | **不碰** | 低于阈值直传原文件 |
| 档位 | **1080p / 720p / 480p** | 见 `transcode_policy.go` 的 DefaultTiers |

实测安装记录：

```
来源    https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip
        （303 → ffmpeg-9.0.1-essentials_build.zip）
大小    111,253,802 字节
落位    myfeed/.run/bin/{ffmpeg,ffprobe,ffplay}.exe
        102,856,192 / 102,652,416 / 104,339,968 字节
版本    ffmpeg version 9.0.1-essentials_build-www.gyan.dev
        built with gcc 16.1.0 (Rev2, Built by MSYS2 project)
编码    --enable-libx264  ← 必须有（H.264 编码是转码的默认输出）
```

⚠ 下载踩过一次坑：第一次跑到 89 MB 时 **curl 超时中断（exit 28）**。
续传要带 `-C -`（服务端支持 Range，实测确实返回 206 只补剩下的 16.54 MB）
外加 `--retry 5 --retry-all-errors`。**别直接重跑整个下载**。

配置里只有 `transcode.ffmpeg_path` 一项（默认 `./.run/bin/ffmpeg.exe`，
跨平台后缀由 `exeSuffix()` 补）。**ffprobe 的路径故意没有加进配置** ——
代码里不用它，它只在验收时手工核对用，加了就是"改了没反应"的假旋钮
（同 `SearchConfig` 那段注释的原则）。

---

## 三、阶段 B：上传质量门禁

一句话：发布时读 mp4 头拿分辨率/码率，决定直传还是入队转码。
**门禁是分流器，不是拒绝器** —— 最坏后果是"这条没转"，不是"用户发不出视频"。

挂在 `Publish` 而不是"上传完成时"：上传和发布是**解耦**的
（文件在 `chunk_handler` 的 `os.Rename` 落地，`videos` 行在 `Publish` 才建），
上传完成时**还没有 videoID**。`Publish` 是唯一的收口点。

### 3.1 验收结果

| 用例 | 期望 | 实测 |
|---|---|---|
| 12 Mbps / 1920×1080 发布 | `transcode_status='pending'` | ✅ `1920x1080@12110kbps 30000ms → pending` |
| 低码率 640×360 / 182 kbps | `'skipped'` | ✅ `→ skipped（shorter than lowest tier (short side 360)）` |
| `src_*` 四列落库 | 与实际文件一致 | ✅ `1920/1080/12110/30000`、`640/360/182/315066` |
| `transcode_status`/`hls_url` 出现在响应里 | 是（前端要靠它选播放模式） | ✅ 响应含 `"transcode_status":"pending","hls_url":""` |
| `src_*` / `probe_status` / `hls_dir` **不出现在响应里** | 是（`json:"-"`） | ✅ 响应里没有这四个字段 |
| 高码率 → 写一条 `video_transcode` outbox 行 | 是 | ✅ 消息落到 MQ（见 3.4） |
| 低码率 → **不写** transcode outbox 行 | 是 | ✅ transcode 队列 0 条 |

### 3.2 ⚠ 修掉的第一个静默 bug：`.run/uploads` 有**六个**来源

加上门禁之前，这个字符串手抄在**五个**地方（两个上传 handler、分片 handler、
头像、`r.Static`）。那时它不发散，因为五处抄的是同一个常量，**谁也没读过配置**。

门禁一来，`probeSource` 开始**从配置读**这个根目录去 `os.Open` 源文件 ——
于是这个字符串第一次有了第六个来源，而它和其他五个**可能不一致**。

不一致的后果是**静默的**：

```
有人在 config.yaml 里写 storage.upload_root: /data/uploads
  写入侧（五个硬编码）→ 文件仍然落到 ./.run/uploads
  探测侧（读配置）    → 去 /data/uploads 找，找不到
表现  门禁对**每一条**视频都"探测失败 → 直传"，transcode_status 一律 'skipped'
      日志是正常的 "probe failed"，**看不出来它一条都没判**
```

已收成唯一来源：`config.StorageConfig` 的 `Root()/VideosDir()/CoversDir()/AvatarsDir()`，
五个写入点全部改调它。`paths_test.go` 的 `TestStorageDirsShareOneRoot`
钉住"所有目录都由 `Root()` 派生"。

### 3.3 ⚠ 修掉的第二个静默 bug：门禁对**全站**静默失效（已实测复现）

**这是本次改动里最值得记的一条**，因为它是"跑起来了、日志正常、功能完全没有"。

第一次跑真实验收时，发布一条能正常解析的素材，拿到的是：

```
[Publish] 门禁 video=http://127.0.0.1:8080/static/videos/10/... 探测=failed 源=0x0@0kbps 0ms → skipped（probe failed）
库里    src_width=0 src_height=0 src_bitrate_kbps=0 src_duration_ms=0
        probe_status=failed  transcode_status=skipped
```

但同一个文件在我的普查里**解析得好好的**。根因：

```
上传接口返回的是 buildAbsoluteURL 拼的**完整 URL**
    http://127.0.0.1:8080/static/videos/10/...
前端原样回传给 /video/publish，Publish 原样落库

而 DiskPath 的第一行是 strings.HasPrefix(urlPath, "/static/")
→ 不匹配 → ok=false → probe failed
```

**真实链路里 `PlayURL` 几乎永远不是 `/static/` 开头。** 所以少了这一段，
门禁对每一条视频都判"探测失败"，而"探测失败"是**设计上完全合法的一个分支**
（真文件不在时就是它）—— 它不会报错、不会告警，只会安静地全部直传。

修法：`DiskPath` 先认绝对 URL（取 `EscapedPath()`），再**统一解码一次**
（`url.PathUnescape`）才做逐段检查。

⚠ 解码这一步是被测试逼出来的：只处理绝对 URL 时，裸路径 `/static/%2e%2e/...`
不解码 → `%2e%2e` 被当成合法文件名原样拼进去。它拼出来**仍在根目录内**
（所以不构成穿越），但它说明**校验看到的字符串和文件系统看到的不是同一个**。
先归一、再判，是安全校验的前提。

`paths_test.go` 现在同时测两种形状，并有一条
`TestDiskPathAbsoluteURLEqualsBarePath` 专门断言"同一资源的两种写法必须解出同一路径" ——
这条如果断了，就是又只修了一种形状。

### 3.4 消息路由：转码事件**没有**被当成时间线事件发出去

计划阶段发现的事实：`OutboxMsg.EventType` 是**死字段** ——
`pollOnce` 只按 `status='pending'` 查，然后**无条件**调 `tmq.PublishVideo`，
它从不读 `EventType`。所以加第二种事件而不改 `pollOnce` 的话，
**转码消息会被当成"视频已发布"发进时间线队列**。

已改成 `OutboxPublishers.deliver()` 按 `EventType` 分流。实测（发布视频 383 之后）：

```
video.transcode.queue        = 1     ← 转码消息到了
video.timeline.update.queue  = 0     ← 没有多出来，没被误发
```

消息体（用管理 API 的 `ack_requeue_true` 取出来看的，**没消费掉**）：

```json
{
  "payload_bytes": 107, "redelivered": false,
  "exchange": "video.transcode.events", "routing_key": "video.transcode.start",
  "delivery_mode": 2, "content_type": "application/json",
  "payload": "{\"event_id\":\"663c2ebe51ca06c883f25b7ad85b5ca3\",\"video_id\":383,\"occurred_at\":\"2026-09-15T04:17:19.7933003Z\"}"
}
```

**只有一个 `video_id`，没有源宽高、没有要生成的档位。** 这是刻意的：
消息会**重投**（at-least-once），重投可能发生在几小时后，那时源信息可能已经变了，
带上旧参数的副本会让 worker 按**过期的输入**去转码，而且不报错。
worker 反正必须读 DB（要改 running/ready、要写 hls_url），顺手多读两列零成本。
一句话：**能重算的都不要传，因为传过来的会过期，重算的不会。**

同时验证**时间线那条路没被改坏**：视频 383 在 Redis 全局时间线 ZSet 里排第一，
score = `1789445839672`（= create_time 的毫秒），ZSet 共 272 条。
`outbox_msgs` 表**空** —— 成功即删（`outboxworker.go` 里是 `Delete`，
不是标记 `sent`），所以它是**队列**不是审计日志。
⚠ 这条会影响验证手法：发布之后 ~1s 内轮询器就把行删了，
想看 outbox 行得在 1s 内查，否则要去 MQ 那边看。

### 3.5 存量素材普查：**0 条会触发转码**

用修好的探针对 `.run/uploads/videos` 下**全部 135 个 mp4** 跑了一遍：

| 项 | 值 |
|---|---|
| 文件数 | 135 |
| 分辨率 | **1920×1080 = 128 条**，640×360 = 5 条，853×480 = 2 条 |
| 短边分布 | 1080 = 128，480 = 2，360 = 5 |
| 码率 | 最小 159 kbps，**最大 1156 kbps** |
| 解析失败 | 0 |
| 尺寸读不出 | 0 |
| **会触发转码** | **0** |

最后一行是"不碰存量"那个决策的机械确认：**135 条里没有一条超过任何档位目标的
1.25 倍**（最低档 480p 的阈值是 1250 kbps，而全库最大只有 1156 kbps）。
所以"不碰存量"不是"我们忍着不动它"，而是**门禁本身就不会碰它**。

### 3.6 探针保真度：和两个独立实现都对过

写 Go 版 `mp4probe.go` 的动机是 `scripts/mp4_probe.py`（那份 Python 是原型）。
**移植必须验证"两边算的是同一个数"**，否则门禁的输入就是可疑的。

**对照实验一：Python 原型，135 个真实素材**

逐文件比对 `(文件名, 字节数, 时长ms)`：

```
135/135 解析成功
135/135 字节数相同
131/135 时长**逐毫秒相同**
  4/135 差**恰好 1 毫秒**
```

那 4 条是**比较口径**的差别，不是解析分歧：Python 原型里是我写的
`int(round(sec*1000))`（四舍五入），Go 版是
`int64(float64(duration)/float64(timescale)*1000)`（**截断**）。
如果 `timescale` 或 `duration` 有一个读错了，差异会是**秒级/小时级**，
不可能是 1 ms。所以两个解析器读到的原始 mp4 字段是一致的。

⚠ 截断而不是四舍五入是**刻意保留**的：截断让 `DurationMS` 偏小 →
码率偏大 → 更容易触发转码。误转的代价是白烧 CPU，漏转的代价是回到
"一个人看都卡" —— 两个方向不对称，所以选保守的那边。

**对照实验二：ffprobe（第三方实现，45 MB 的 12 Mbps 素材）**

| | 本探针 | ffprobe |
|---|---|---|
| 宽高 | 1920×1080 | 1920×1080 |
| 时长 | 30000 ms | 30.000000 s |
| 码率 | **12110 kbps** | **12,110,004 bps** |

码率精确吻合（12110.004 → 截断 12110）。
注意单位：ffprobe 给的是 **bps**，本探针存的是 **kbps 整数** ——
把 bps 当 kbps 写进断言会差 1000 倍。

**这个素材同时补了一个覆盖盲区**：它是 `-movflags` 默认（**moov 在文件尾**）
生成的，box 顺序是 `ftyp free mdat moov`：

```
ftyp   off=0          size=32
free   off=32         size=8
mdat   off=40         size=45401339
moov   off=45401379   size=11139      ← 在 45 MB 之后
```

而**存量 135 个素材全部是 faststart（moov 在头）**。也就是说只用真素材验证的话，
"moov 在尾"这条路径**从来没被走过** —— 而它恰好是有人直接传手机原片时的布局，
正是门禁最想拦的那一类。

已固化成 `internal/video/testdata/` 下**两个刻意造的 fixture**：

| fixture | box 顺序 | 对应现实 |
|---|---|---|
| `moov_at_end.mp4` | `ftyp free mdat moov` | 未优化的相机输出 |
| `faststart.mp4` | `ftyp moov free mdat` | 和存量 135 个一样 |

两个**同尺寸、同时长、连字节数都一样（24064）**，唯一的区别就是 box 顺序 ——
这样"两个都解得出来且结果相同"就只可能归因于解析器真的按 box 链去 seek 了。
期望值全部取自 ffprobe 的独立输出（320×240 / 1000ms / 192512 bps）。

### 3.7 `transcode_status` 有一个**第六取值**：空串

实测 `select transcode_status, count(*) group by 1`：

```
（空串）  127 条
skipped     1 条
```

那 127 条是**本项目改造之前**发布的视频 —— 这一列是跟门禁一起加上去的，
ALTER TABLE 给的默认值就是空串，**存量行不回填**。

给它起了名字 `TranscodeUnknown`，因为它是一个**真实且会长期存在**的状态：
"事实直传"和"门禁判定直传"是两回事，把存量记成 `skipped` 等于伪造了一次判定。

不影响功能：播放端只有 `== "ready"` 才走 HLS，空串自然走直传老路；
过载降级只统计 HLS 会话，也不会碰它们。

---

## 四、阶段 C：转码 worker + 多档 HLS

### 4.1 一个布局决定，取代了计划里的一条纪律

计划原本的输出布局是 `hls/{id}/{1080p,720p,480p}/`，配套要求写的是
**"重跑前必须先清掉半成品输出目录，否则 master 会指向半截分片"**。

那条要求是对的，但它是在**管理**一个本可以不存在的危险。实际布局多加了一层
一次转码一个的 runID，`master.m3u8` 成为唯一稳定 URL：

```
hls/383/master.m3u8            ← 稳定；每次转码完**最后**重写（提交点）
hls/383/dc3540bdac0e6cf2/1080p/index.m3u8 + seg_00000..4.ts
hls/383/dc3540bdac0e6cf2/720p/...
hls/383/dc3540bdac0e6cf2/480p/...
```

这三件事因此**在构造上不可能发生**，不再依赖谁记得清目录：

| 原危险 | 加一层之后 |
|---|---|
| 同 URL 下分片内容会变 → `.ts` 的 `immutable` 会和新分片混用 | 每次 runID 不同 → **同一 URL 下的字节永不改变**，`.ts` 才敢真的 immutable |
| 转码中途被杀 → master 指向半截产物 | 产物全部写完 + 校验通过才重写 master → master 任何时刻只指向一棵**完整**的树 |
| 失败前必须先清目录（破坏性操作） | 旧 run 目录只在**成功之后**删，删的是"没人再引用的 URL" → 清理变成非破坏性的 |

副产物：两个 worker 同时转同一条视频也是安全的（各自的 runID 互不覆盖）。

### 4.2 验收结果

两条素材，都是 12 Mbps / 1920×1080，**唯一的差别是有没有音轨**：

| | #383 | #384 |
|---|---|---|
| 源 | 45 MB，**无音轨**（lavfi testsrc2） | 30.9 MB，AAC 192k |
| runID | `dc3540bdac0e6cf2` | `b7f9a9f6800fdf2f` |
| 耗时 | 5.308 s | 3.255 s |
| 磁盘占用 | **29.3 MB / 19 文件** | **20.6 MB / 16 文件** |

`ffprobe` 三档（分辨率 / 实测均值 kbps / 目标 kbps）：

| 档 | 分辨率 | #383 | #384 | 目标 | 音轨 |
|---|---|---|---|---|---|
| 1080p | 1920×1080 | 4552 | 4690 | 4628 | #384 有 |
| 720p | 1280×720 | 2591 | 2737 | 2628 | #384 有 |
| 480p | **854×480** | 1058 | 1204 | 1128 | #384 有 |

- 480p 是 **854** 不是 852 —— `evenAlign(v) = (v+1) &^ 1` 和 ffmpeg 的
  `-2` 对齐规则（`FFALIGN(v,2)`）一致，和 `ffprobe` 对过。
- `#384` 三档全部含 `aac,audio,48000,2`，确认 `-map 0:a:0?` 的**后半段**
  （有音轨时音轨在不在）也是对的。`#383` 只覆盖了前半段（没有音轨也不失败）——
  这条素材没有任何音轨，所以它验证不了音轨。**两条都要有，缺一条就是半个证据。**
- 实测码率 / 目标码率落在 **93.8% ~ 106.7%**，见 §4.6。

HTTP 层（`internal/http/static.go` 替换掉 `r.Static` 之后，实测响应头）：

| 路径 | Cache-Control | Content-Type |
|---|---|---|
| `master.m3u8` | `no-cache`（+ `ETag` → **304**） | `application/vnd.apple.mpegurl` |
| `{runID}/1080p/index.m3u8` | `no-cache` | `application/vnd.apple.mpegurl` |
| `{runID}/1080p/seg_00000.ts` | `public, max-age=31536000, immutable` | `video/mp2t` |
| 直传 `.mp4` | `public, max-age=86400` | `video/mp4` |

`Range: bytes=0-1023` → **206** + `Content-Range: bytes 0-1023/18812047`；
越界 → **416**；`/static/../configs/config.yaml` 及两种编码变体 → **404**。

`.m3u8` 用 `no-cache` 而**不是** `no-store`：后者连 304 都拿不到，
每次切档要重下整份清单。这条有专门的命名测试钉着（`TestPolicyForM3u8IsNotNoStore`）。

### 4.3 ⚠ 静默 bug #3：gin 的 wildcard 参数**自带前导斜杠**

`config.StaticURLPrefix` 以 `/` 结尾，gin 的 `*filepath` 参数以 `/` 开头，
直接相加得到 `/static//hls/383/...`。那个空段会被 `DiskPath` 拒掉
（它显式拒绝空段），于是**每一个静态资源 404**。

这条本身一眼可见（全站白屏），所以它反而是最不打紧的一种 ——
列在这里是因为它的**根因**值得记：两个"都带斜杠"的字符串相加。

### 4.4 ⚠ 静默 bug #4：`DiskPath` 不读 `Root()` 的兜底 → 全站 404 且零日志

`StorageConfig` 有两个访问器，只有**一个**做了空值兜底：

```go
func (s StorageConfig) Root() string {
    if s.UploadRoot == "" { return DefaultUploadRoot }   // ← 兜底
    return s.UploadRoot
}
```

而 `DiskPath` 的第三步（"拼完之后再确认真的落在根目录里"）读的是
`s.UploadRoot`。`UploadRoot` 为空时 `filepath.Clean("")` = `"."`，
于是 `HasPrefix(full, "."+sep)` 对**每一个**路径都失败 ——
**所有静态资源 404，没有任何一行日志。**

正常链路上 `config.Load` 的 `withDefaults` 会填上它，所以这条在真实运行中
不会出现。它是**测试逼出来的**：我原本写的断言是错的（断言了默认路径的
字面量），改成"`StorageConfig{}` 和 `{UploadRoot: DefaultUploadRoot}` 必须给出
同一个答案"之后，这个分叉立刻红了。这说明原来的断言不只没用，还**挡住了**它要找的东西。

### 4.5 ⚠ 静默 bug #5：转码成功，但 `hls_url` **永远到不了前端**

这是本次最值钱的一条，因为它属于"转码白做了"却完全不报错的形状。

**机制**：`getDetail` 有阶段7的旁路缓存（`v1:video:detail:id=<id>`，TTL 5min）。
转码 worker 的 `MarkTranscode` 只写了 MySQL，**谁也没删这两个 key**：

```
v1:video:detail:id=<id>   video 包的 getDetail 响应缓存（TTL 5min）
v1:video:entity:<id>      feed 包的 GetVideoByIDs L2 实体缓存（TTL 1h）
```

**触发窗口不是边角情况，而是最常见的那条路**：

```
发布 → 行是 pending / hls_url='' → 上传者立刻打开自己的详情页
     → 这份 pending 被回填进缓存 → 转码完成，DB 变成 ready
     → 但缓存还是 pending，5 分钟内所有人拿到的 hls_url 都是空串
     → 前端走直传 mp4 老路，HLS 产物**一次都不会被请求**
```

**实测复现**（往 `v1:video:detail:id=383` 种一份 `pending`，DB 是 `ready`）：

```
缓存里:      "transcode_status":"pending"  "hls_url":""
getDetail:   "transcode_status":"pending"  "hls_url":""     ← 脏值被直接发出去
```

⚠ 复现时**第一次种错了 key**（`v1:video:detail:383`，漏掉 `id=`）——
key 里的 `id=` 是格式串 `"video:detail:id=%d"` 的一部分，不是拼出来的标记。
种错的表现是"请求返回了正确值"，看起来像"这个 bug 不存在"。
**手工构造缓存 key 之前必须先去 Redis 里 `KEYS` 看一眼真实形状。**

**修法**：不是"加一行记得删缓存"，而是把写 DB 和删缓存**绑成一个动作**
（`TranscodeWorker.markTranscode` 成为唯一出口，三个调用点全部改走它），
并且把那一对 key 收进 `video.InvalidateVideoCaches` 单一来源 ——
`UpdatePopularityCache` 里原本内联着同一对 key，现在也改调它了。

这条和 §4.1 是同一个思路：**靠结构，不靠纪律。**

### 4.6 码率保真度：实测 vs 声明（ABR 的误差带）

| | 均值/目标 | 峰值/均值 | 峰值/声明 |
|---|---|---|---|
| #383 1080p / 720p / 480p | 98.4% / 98.6% / 93.8% | 101% / 101% / 103% | 100.1% / 99.6% / 96.9% |
| #384 1080p / 720p / 480p | 101.3% / 104.1% / **106.7%** | 100% / 100% / 103% | 101.9% / 104.5% / **110.0%** |

两条结论：

1. **ABR 是可预测的**（整个扩展立在这上面）：误差带 **93.8% ~ 106.7%**，
   峰值段不超过均值的 103%。CRF 做不到这一点 —— 那样
   "Σ档位码率"这套负载核算就是在编一个没人能证伪的数。
2. 但**声明值（`BANDWIDTH`）偏乐观**：最坏一格（480p 带音轨）的
   **峰值段是声明值的 110%**。HLS 规范里 `BANDWIDTH` 应当填**峰值段**码率，
   而我们填的是档位表的**目标**值。码率越低，MPEG-TS 打包开销占比越高，
   这个偏差越大（1080p 几乎无偏差，480p 到 10%）。

   → **阶段 E 的预算核算不能直接用声明值**，否则每一档低估 0~10%。
   要么按实测上浮、要么把真实峰值段码率算出来写进 master。
   这是阶段 E 开工前必须先定的一件事，记在这里免得忘。

### 4.7 幂等读的是**产物**，不是数据库

重投一条 383 的消息（`video.transcode.start`），worker 日志：

```
video=383 就绪 3 档 耗时=7ms 占用=29.3MB/19 文件
         url=/static/hls/383/master.m3u8（产物已在磁盘上，跳过转码）
```

**7ms vs 首次 5.308s**，转码进程数全程为 0，且 master 指向的那棵 run 目录
（`dc3540bdac0e6cf2`）安然无恙 —— 残留 run 目录数 = **1**。

为什么判断走的是 `VerifyMaster`（读磁盘上的 master，确认每一档的 playlist 非空）
而不是查 `transcode_status == 'ready'`：**DB 那一半正是可能写失败的那一半，
磁盘那一半才是已经成功的那一半。** 转码完成、master 写好、但 `MarkTranscode`
失败时，库里是 `running` 而磁盘上是完整产物 —— 只有读产物才能认出这种状态
并直接跳过重跑。

好消息：这次重投**顺带验证了 §4.5 的修复**——重投前种进去的两个脏 key
（`detail` 和 `entity`）在重投后 `EXISTS` 都变成 **0**。

---

## 五、阶段 D：播放端 hls.js + ABR

### 5.1 两条路并存，而不是替换

`hls_url` 非空 → hls.js；空 → 保持原来的 `<video :src>` 直传。
判据**只认 `hls_url` 一个字段**，不再去看 `transcode_status` ——
后端的不变量是"这一列只在 ready 时才非空"，判两次只会多出一个
两边可能不一致的分支。落在老路上的有三类：127 条存量（状态是空串）、
门禁判成 `skipped` 的、转码 `failed` 的。**它们的播放行为一个字都没变。**

### 5.2 ⚠ 最重要的一个决定：hls.js 延迟到按播放才创建

hls.js 默认 **attachMedia + loadSource 之后立刻开始拉清单和分片**。
照默认写的话，VideoPlayer 顶上那 16 行关于 `preload` 的注释就全废了 ——
而且更糟：它不是 `<video>` 的一个属性，是 hls.js 的默认值，**DevTools 里
看不出是"谁"在拉**。

`autoStartLoad: false` + 按播放再 `startLoad()` 是 hls.js 的正规写法，
但它**是否真的不取 master** 取决于版本实现（清单往往在 `loadSource` 就取了）。
所以这里用的是延迟**构造**：不构造 = 一个字节都不会发出去。
和 §4.1 的 runID 是同一条判断 —— **结构上保证，而不是靠一个开关的语义。**

代价写明：按播放后要先等一次 master 的 RTT，首帧比直传那条路晚。
和 preload 那次一样，**白传的字节 ↓、首帧 ↑**，两个数要一起看。

### 5.3 hls.js 用动态 import（顺手修掉的一个 575 kB）

第一版是顶部 `import Hls from 'hls.js'`，构建出来：

```
VideoDetailView-*.js   597.46 kB │ gzip: 187.20 kB     ← 改之前是 21.74 kB
```

也就是说**每一条**视频的详情页都要先下完 575 kB 的 hls.js 才能开始播 ——
包括那 127 条存量。改成 `await import('hls.js')` 之后：

```
VideoDetailView-*.js    22.58 kB │ gzip:   8.79 kB
hls-*.js               574.74 kB │ gzip: 178.99 kB     ← 只在真的播 HLS 时才拉
```

这和这个文件开头的 preload 那段是**同一个判断**，只是省的是 JS 字节而不是媒体字节。

⚠ 由此带出一个必须一起处理的时序问题：动态 import 是异步的，
所以 `play()` 里**不能**先 `startHls()` 再无条件 `v.play()` ——
那一刻元素还没有 src（HLS 模式下 `:src` 是 `undefined`），
`play()` 会被浏览器拒掉，而 hls.js **不会**帮你起播
（`autoStartLoad` 管的是加载，不是播放）。第一版就是这么写的，
表现是**按了播放键画面不动、进度条不走、一个请求都没发**。
现在改成 `startHls().then(() => v.play())`。

### 5.4 请求链实测（走 vite 代理，和浏览器同一条路）

```
① /static/hls/383/master.m3u8               → dc3540bdac0e6cf2/1080p/index.m3u8
② /static/hls/383/<runID>/1080p/index.m3u8  → 5 个分片，VOD，EXT-X-INDEPENDENT-SEGMENTS
③ .../1080p/seg_00000.ts                     → 200，3442468 字节，video/mp2t
④ playlist 声明的段数 5 = 磁盘上实际 5
```

相对解析是对的（hls.js 按 master 所在目录解析档位 URL，所以 runID 一直
只出现在 url 的**中段**，前端从头到尾不知道它是什么）。

### 5.5 ABR 埋点接线

hls.js 的 `LEVEL_SWITCHED` → `useQoE.setLevel(level.bitrate / 1000)`，
于是 `qoe_events.avg_bitrate_kbps` 是**按时长加权**的均值，
`tier_switches` 数的是切换次数（而不是首尾档位比较 ——
来回横跳最后回到原档位会被首尾比较抹成 0 次，而那恰恰是 ABR 最该被批评的行为）。
`bitrate` 是 bps、表里存 kbps，**口径转换只在这一处做**。

`Hls.Events.ERROR` **只有 `data.fatal` 才切错误态**：非 fatal 的每次网络抖动
都会来一条，拿它切错误态会让播放器无缘无故变成"打不开"。

---

## 六、阶段 E：过载降级

_待做_

---

## 七、诚实的边界

- 门禁的判定用的是**文件大小 / 时长**算出的平均码率，不是 ffprobe 那种
  精确的轨道码率。对"要不要转码"这个二值判断足够（阈值差着上千 kbps），
  但它**不能**用来做任何需要精度的分析。
- §3.5 的普查结论只对**当前这 135 个文件**成立。新上传的素材会不会触发转码，
  由上传者决定 —— 这正是这个扩展要改变的现状。
- 阶段 E 的降级效果最终要**转回 QoE 对比**才有意义
  （同样并发下 降级开 vs 关 的 `stall_count` / `stall_total_ms`），
  而不是"日志里看到触发了"。见 `bench-notes-qoe.md` 的基线约束。
- **`BANDWIDTH` 填的是目标值，不是峰值段码率**（§4.6 第 2 条）。实测最坏能到
  声明值的 110%。它影响两件事：hls.js 选档的保守程度、阶段 E 的负载核算。
  在阶段 E 之前必须收敛成一个明确的口径。
- **`transcode_status` 的"真相"有两份副本**（MySQL + Redis 里那两个 key）。
  §4.5 修掉的是"没人删"，但结构本身还在：**任何将来直接写 `videos` 表、
  绕过 `markTranscode` 的代码，都会重新制造这个 bug**，而且同样不报错。
  唯一的防线是那条规则：改 Video 就调 `InvalidateVideoCaches`。
- **§4.2 的分片时长固定 6s**，而码率是**平均**概念。真实播放器按 6s 一段拉，
  所以"码率 × 时长 = 流量"只在这个粒度上成立 —— 突发集中在单个分片里
  时（峰值/均值 103%）会短暂超出。
- ⚠ **`bench-notes-qoe.md` §4.3 要求的 QoE 基线这次被错过了。**
  它要求基线在 `r.Static` → `mountStatic` 替换**之前**采集，
  因为给已有的 135 条直传 mp4 加上 `Cache-Control` 会改变**重复播放**的行为
  （第二次播放不再走网络）。替换已经写完了，基线还没采。
  补救办法仍在：临时回退成 `r.Static` 跑一次基线，再切回来 ——
  但**基线只能对着旧构建采**这件事本身说明：顺序错了就没有"同一台机器上
  同一时刻"的对照。真实浏览器播放无法脚本化，所以这一段始终要手工补。
- ⚠ **阶段 D 的验收里，"浏览器打开能播"这一条没做。** 已经实测的是
  请求链（§5.4：master → 档位 → 分片，全部 200 且段数对账）和构建产物
  （chunk 拆分、typecheck 通过）。**没有验证**的是：hls.js 真的起播、
  切档真的发生、`qoe_events.avg_bitrate_kbps` 和档位对得上。
  这三件都需要真实浏览器，脚本替代不了。
- ⚠ **`startup_ms` 的口径问题**（见 `useQoE.ts` 里的注释）：起点是**组件挂载**
  而不是"按下播放"。用户按得越快偏差越小，按得越慢越是纯噪声。
  这是**定义问题**而不是实现问题，所以没有擅自改 —— 一改，已记录的数据就不可比了。
  阶段 E 的降级对比用 `stall_count` / `stall_total_ms`，不受它影响。

