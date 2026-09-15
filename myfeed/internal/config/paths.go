package config

import (
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// StaticURLPrefix 静态资源的 URL 前缀。
//
// 它在这里定义、由 router.go 挂载，是因为**这个前缀和磁盘布局是一件事的两面**：
//
//	URL  /static/videos/1/20260915/abcd.mp4
//	磁盘 <UploadRoot>/videos/1/20260915/abcd.mp4
//
// 前缀写在 router.go、根目录写在别处的话，两者之间的映射就成了
// 第三份需要手工同步的知识 —— 而它一旦错了，表现是"文件明明在，就是 404"。
const StaticURLPrefix = "/static/"

// DiskPath 把一个静态资源的 URL 路径换成磁盘路径。
//
// 返回 ok=false 表示**这个路径不归 UploadRoot 管**（不是 /static/ 开头），
// 或者**它试图跳出根目录**。两种情况调用方都必须当成"没有这个文件"，
// 而不是"拼一个凑合的路径试试"。
//
// ---------- 为什么必须防穿越 ----------
//
// `PlayURL` 是**客户端传进来的**：PublishVideoRequest.PlayURL 从 JSON 直接
// 绑进来，Publish 只做了 strings.TrimSpace。所以在加这个函数之前，
// 一个请求完全可以发布 `play_url: "/static/../../../../Windows/win.ini"`。
//
// 这个洞在**本函数出现之前就存在**（r.Static 也吃这个路径），
// 但那时它只是一条 URL；现在我们要**按这个路径去 os.Open 文件**读它的头 ——
// 同一个洞的后果从"能请求到一个 URL"变成了"能读到任意本地文件的信息"。
// 所以这里显式挡掉，而不是依赖"反正 gin 会 clean"。
//
// 判据是**先逐段拒绝 `..` 再 clean**，而不是"clean 完看看有没有 .."：
// path.Clean("/a/../../b") 会得到 "/b" —— 穿越的痕迹被 clean 抹掉了，
// 事后检查是查不出来的。必须在 clean 之前查。
func (s StorageConfig) DiskPath(urlPath string) (string, bool) {
	// ⓪ 先接受**绝对 URL**。这一条不是可选的洁癖，它来自一次实测翻车：
	//
	// 上传接口返回的是 buildAbsoluteURL 拼出来的完整 URL
	// （`http://127.0.0.1:8080/static/videos/10/...`），前端原样回传给
	// `/video/publish`，Publish 原样落库。所以**真实链路里 PlayURL 几乎
	// 永远不是 `/static/` 开头**，而是 `http://host:port/static/`。
	//
	// 少了这一段，DiskPath 对**每一条**视频都返回 ok=false → 门禁全部走
	// "探测失败 → 直传" → 全站 transcode_status 恒为 'skipped'。
	// 日志是正常的 "probe failed"，看不出来 —— 门禁静默失效。
	//
	// 注意这里**不校验 host**：`http://evil.com/static/videos/1/x.mp4`
	// 会被解析成我们本地的同相对路径。这没有安全问题（它只是让我们去读
	// 自己磁盘上一个同样相对路径的文件），而是刻意的宽容 ——
	// 换个域名/端口部署时不必同步改配置。
	//
	// 取 EscapedPath() 而不是 Path：Path 是**已解码**的，下面还要统一解码一次，
	// 那样 `%252e%252e` 会被解两遍变成 `..` —— 双重解码是经典的绕过手法，
	// 这里恰好让它更严（多拒一点），但"靠巧合更严"不是能写进代码的性质。
	// 一次解析、一次解码，每个输入只解码一次，才说得清。
	if i := strings.Index(urlPath, "://"); i > 0 {
		u, err := url.Parse(urlPath)
		if err != nil {
			return "", false
		}
		urlPath = u.EscapedPath()
	}

	// ⓪' 统一解码一次（两种形状都走这里）。
	//
	// **不能跳过这一步**：`%2e%2e` 在磁盘上是个合法文件名字符，
	// 不解码的话它会被当成普通段原样拼进路径 —— 虽然拼出来仍在根目录内
	// （所以不构成穿越），但它说明**校验看到的东西和文件系统看到的东西
	// 不是同一个字符串**。安全校验必须建立在这个前提上：先归一，再判。
	//
	// 解不开（`%` 后面不是合法十六进制）就直接拒 —— 我们自己的上传路径是
	// 16 字节 hex + 扩展名，不可能含 `%`，所以拒绝它不会误伤任何真实数据。
	decoded, err := url.PathUnescape(urlPath)
	if err != nil {
		return "", false
	}
	urlPath = decoded

	if !strings.HasPrefix(urlPath, StaticURLPrefix) {
		return "", false
	}
	rel := strings.TrimPrefix(urlPath, StaticURLPrefix)
	if rel == "" {
		return "", false
	}

	// ① clean 之前逐段查 ".."，还有空段和 "." 也一并拒掉 ——
	//    正常的 URL 里不该有它们，出现就是有人在试探。
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return "", false
		}
	}

	// ② clean 一次（去掉重复斜杠等），注意用 path 而不是 filepath：
	//    这里的输入是 **URL 路径**，分隔符恒为 "/"，用 filepath 在 Windows 上
	//    会把 "/" 当合法文件名字符而完全不清洗。
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	if rel == "" {
		return "", false
	}

	// ③ 兜底：拼完之后再确认一次真的落在根目录里。
	//    前面两条已经够了，这一条是"双保险"——它防的是我上面没想到的第三种写法。
	//    安全边界的代码值得多花三行。
	//
	// ⚠ 走 s.Root() 而**不是** s.UploadRoot。这两个在正常路径下是同一个值
	// （config.Load 的 withDefaults 会填），差别只在 UploadRoot 为空时 ——
	// 而那正是它要紧的地方：直接用空字符串的话 root = "."，
	// 下面的 HasPrefix 检查会对**每一个**路径都失败，
	// 表现是"所有静态资源 404"而没有任何一行日志。
	// Root() 的兜底（见它的注释）在这里必须同样生效，
	// 否则"有一个访问器兜底、另一个不兜底"就成了一个新的静默分叉。
	root := filepath.Clean(s.Root())
	full := filepath.Join(root, filepath.FromSlash(rel))
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

// HLSRelDir 某条视频的 HLS 产物相对于 UploadRoot 的目录（用 "/" 分隔）。
//
// 只此一处定义，因为它的两个读者必须完全一致：
//
//	转码 worker  → 把产物写到这里
//	API 进程     → 从这里读 master.m3u8（阶段 E 现场生成）和 .ts 分片
//
// 两边各写一遍路径模板的话，错了的表现是"转码成功但播放 404"。
func HLSRelDir(videoID uint) string {
	return "hls/" + strconv.FormatUint(uint64(videoID), 10)
}

// HLSMasterName master playlist 的文件名。
const HLSMasterName = "master.m3u8"

// HLSMasterURL 某条视频 HLS 播放列表的 URL **路径**，也就是落进
// `videos.hls_url` 那一列的值。纯字符串拼接、不碰配置 —— 它是 URL 的形状，
// 和磁盘布局无关（磁盘布局那半在下面三个方法里）。
//
// 它现在**只在这里定义一次**，因为有两个读者：
//
//	转码 worker  → 转完之后写进 DB
//	API 进程     → 和 DB 里存的值比对（阶段 E 的动态 master 要反查 videoID）
//
// ⚠ 它返回的是**路径**（`/static/hls/383/master.m3u8`），不是完整 URL。
// 这是刻意的，理由和 `entity.go` 里 HlsURL 那段注释一致：**将来换域名/上 CDN 时
// 只有一处要改**（前端拼 base），落库的值不会变成一堆写死的 `http://127.0.0.1`。
func HLSMasterURL(videoID uint) string {
	return StaticURLPrefix + HLSRelDir(videoID) + "/" + HLSMasterName
}

// ---------- 一次转码运行一个目录（阶段 C 的核心布局决定）----------
//
// 产物不是直接写在 `hls/<id>/` 下，而是写在 `hls/<id>/<runID>/` 下，
// **master.m3u8 是唯一一个稳定 URL**：
//
//	hls/383/master.m3u8              ← 稳定，每次转码完被重写（内容指向下面某个 run）
//	hls/383/9f3a1c02/master.m3u8     ← 上一次的
//	hls/383/9f3a1c02/1080p/index.m3u8
//	hls/383/9f3a1c02/1080p/seg_00000.ts
//	hls/383/a71b4e55/...             ← 这一次的
//
// ---------- 为什么值得多一层目录 ----------
//
// 计划里原本的布局是 `hls/<id>/{1080p,720p,480p}/`，配套要求是
// "重跑前必须先清掉半成品输出目录，否则 master 会指向半截分片"。
// 那个要求是对的，但它是在**管理**一个本可以不存在的危险：
//
//	同一个 URL 下的分片内容会变 → 浏览器对 .ts 设的 immutable 缓存
//	                              会和新分片混用 → 播放错乱，且不报错
//	转码中途被杀                  → 目录是半截的，而 master 可能已经指过去了
//
// 加一层 runID 之后这两件事**在构造上不可能发生**：
//
//	分片 URL 的唯一性   每次转码的 runID 不同 → 同一个 URL 下的字节永远不变，
//	                    `.ts` 才敢真的设 immutable（这是它敢长缓存的前提）
//	半截产物不可达      新产物全部写完、校验通过之后才重写 master（提交点），
//	                    master 在任何时刻都只指向一棵**完整的**产物树
//	清理变成非破坏性的  删旧 run 目录只影响"没人再引用的 URL"，
//	                    所以在成功之后删，而不是失败之前清
//
// 代价是多一层目录、多一个要理解的概念。换来的是"转码幂等"这件事
// 从"靠纪律"变成"靠结构" —— 而这个项目的纪律已经丢过一次消息了。

// HLSRoot 某条视频 HLS 产物的根目录（磁盘路径）：`<UploadRoot>/hls/<id>/`。
//
// `hls_dir` 那一列存的就是它。
func (s StorageConfig) HLSRoot(videoID uint) string {
	return filepath.Join(s.Root(), filepath.FromSlash(HLSRelDir(videoID)))
}

// HLSRunDir 某一次转码运行的产物目录（磁盘路径）。
func (s StorageConfig) HLSRunDir(videoID uint, runID string) string {
	return filepath.Join(s.HLSRoot(videoID), runID)
}

// HLSMasterDiskPath master.m3u8 的磁盘路径。
//
// ⚠ 它和 HLSMasterURL 是同一件事的两面（URL ↔ 磁盘），
// 所以两个函数放在一起 —— 分开放的话，"URL 是 /static/hls/<id>/master.m3u8
// 而磁盘是 <root>/hls/<id>/master.m3u8"这个映射就有了两个各自为政的版本。
func (s StorageConfig) HLSMasterDiskPath(videoID uint) string {
	return filepath.Join(s.HLSRoot(videoID), HLSMasterName)
}

// ---------- 上传落地的目录布局：只此一处 ----------
//
// ---------- 为什么要从"五处硬编码"收回到这里 ----------
//
// 加上门禁（阶段 B）之前，`.run/uploads` 这个字符串**手抄在五个地方**：
//
//	r.Static("/static", "./.run/uploads")        router.go
//	videos/<id>/<date>/                          video_handler.go 的直传
//	                                             video_handler.go 的封面
//	                                             chunk_handler.go 的分片
//	avatars/<id>/                                account/handler.go
//
// 那时它不发散，因为五处抄的都是**同一个常量**，谁也没读过配置。
// 但门禁一来，`probeSource` 开始**从配置读**这个根目录去 os.Open 源文件 ——
// 于是这个字符串第一次有了第六个来源，而它和其他五个可能不一致。
//
// ---------- 不一致的后果（这是它必须被修掉的原因）----------
//
// 有人在 config.yaml 里写 `storage.upload_root: /data/uploads`：
//
//	写入侧（五个硬编码）→ 文件仍然落到 ./.run/uploads
//	探测侧（读配置）    → 去 /data/uploads 找，找不到
//
// 表现**不是报错**，而是每一条视频的探测都失败 → 门禁全部走"探测失败 → 直传"
// → `transcode_status` 一律 'skipped'，日志里是正常的 "probe failed"。
// **门禁看起来在工作，其实一条都没判。** 这正是本项目最忌讳的那类失败。
//
// 所以下面两个函数把布局收成唯一来源，五个写入点全部改用它。
// 这不是"顺手重构"——它是让门禁能真正生效的前提。

// VideosDir 某个账号在某天上传视频的落地目录：`<UploadRoot>/videos/<账号ID>/<日期>/`。
//
// 按日期分目录是老约定（避免单目录塞几十万文件），这里保持不变。
// date 用 `20060102` 格式，调用方各自 time.Now().Format 后传进来 ——
// 本函数**不读时钟**，因为"哪一天的目录"是调用方的语义，不是路径布局的事
// （转码 worker 将来按 video.create_time 的日期找文件时，
// 它必须能传一个不是"今天"的日期）。
func (s StorageConfig) VideosDir(accountID uint, date string) string {
	return filepath.Join(s.Root(), "videos", strconv.FormatUint(uint64(accountID), 10), date)
}

// AvatarsDir 某个账号的头像目录：`<UploadRoot>/avatars/<账号ID>/`。
func (s StorageConfig) AvatarsDir(accountID uint) string {
	return filepath.Join(s.Root(), "avatars", strconv.FormatUint(uint64(accountID), 10))
}

// CoversDir 某个账号某天的封面目录：`<UploadRoot>/covers/<账号ID>/<日期>/`。
//
// 封面和视频分开两棵子树（不是 videos 下的子目录），因为它们的生命周期不同：
// 视频会被转码、会被清理任务扫；封面不会，而且封面的 URL 要能在视频被删之后
// 继续可用（列表页占位）。
func (s StorageConfig) CoversDir(accountID uint, date string) string {
	return filepath.Join(s.Root(), "covers", strconv.FormatUint(uint64(accountID), 10), date)
}

// Root 上传根目录的磁盘路径。**每个写入点都必须经过这里，不要自己拼字符串。**
//
// 空值时回落默认值是**故意**的，虽然 config.Load 的 withDefaults 已经填过了：
// 手写 &Config{} 或 &StorageConfig{} 的地方（测试、小工具）
// 会静默拿到空字符串，而 filepath.Join("", "videos") = "videos" ——
// 于是文件落到**当前工作目录**下，不报错，只是找不到了。
// 让访问器自己兜底，比指望每个构造点都记得调 withDefaults 可靠。
func (s StorageConfig) Root() string {
	if s.UploadRoot == "" {
		return DefaultUploadRoot
	}
	return s.UploadRoot
}
