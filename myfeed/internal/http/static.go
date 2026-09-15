package http

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"myfeed/internal/config"

	"github.com/gin-gonic/gin"
)

// 本文件替代 gin 的 `r.Static("/static", root)`。
//
// ---------- 为什么非换不可 ----------
//
// 阶段 C 的核心收益是"播放端按档位拉分片"，而它依赖两类**完全相反**的
// 缓存策略同时成立：
//
//	.m3u8  播放列表  → 不能缓存（重写 master 是转码和降级的提交点，
//	                  拿到一份旧的等于拿到一份指向已删产物的清单）
//	.ts    HLS 分片  → 必须长缓存（一条视频几十上百个分片，
//	                  每个都回源等于每次播放都重传整条视频）
//
// gin 的 Static 会把**同一套**头给所有文件，做不到按扩展名区分。
// 这不是"配置一下就行"的事 —— 它有且只有一种策略。
//
// ---------- 换掉它必须自己接手的四件事 ----------
//
// 手写一个静态文件处理器**很容易只做出"能返回文件"这一个功能**，
// 而 r.Static 顺带给了另外四件，任何一件漏掉都是回归：
//
//	① 穿越防护      gin 走 http.Dir，自带 clean + 拒绝跳出根目录。
//	                这里改走 config.DiskPath —— 本项目自己那份带测试的
//	                唯一 URL→磁盘 映射（paths_test.go 有 19 个用例，
//	                其中两个专门测穿越）。**不自己写第二份守卫**：
//	                两份守卫的结果不一致时，没有任何东西会发现。
//	② Range 请求    没有它，直传的 135 条 mp4 **拖进度条就播不动**。
//	                http.ServeContent 自带（它同时管 206 和 Accept-Ranges）。
//	③ ETag/304      配合 Cache-Control 让"没变的东西不重传"真正生效。
//	④ 正确 MIME     .m3u8/.ts 不在 Go 的 mime 表里，不显式给的话
//	                ServeContent 会去**嗅探内容**，.m3u8 会被判成
//	                text/plain。hls.js 不在乎，但浏览器原生 HLS 在乎 ——
//	                那是 iOS Safari 上唯一的播放路径。
//
// 所以这里用 http.ServeContent 而不是自己 io.Copy：Range/ETag/Last-Modified
// 这三块都是"写起来简单、写对了很难"的代码（RFC 7233 的条件请求有十几种
// 组合），标准库那份是被全世界测过的。
//
// ---------- 一个副作用：这是本项目第一次让存量 mp4 有缓存头 ----------
//
// 在此之前 .mp4 没有任何 Cache-Control，浏览器只能按启发式规则猜。
// 加上之后重复播放会命中缓存、不再整份重拉 —— 这会**改变 QoE 的数字**
// （启动更快、白传字节更少）。所以 bench-notes-qoe.md 的 §4.3 明确要求
// 基线必须在这次替换**之前**采。这不是一个可以事后补的前提。

// StaticPolicy 一个静态文件的响应策略。
//
// 抽成**纯函数**（只吃文件名，不碰请求、不碰配置）是为了能直接单测 ——
// 这张表上任何一行写错，表现都是"某一个扩展名的资源缓存行为不对"，
// 而它在浏览器里**看不出区别**（缓存策略错了只会表现为"有时候拿到旧的"，
// 而"有时候"恰恰是最难复现的）。
type StaticPolicy struct {
	ContentType  string
	CacheControl string
}

const (
	// cacheImmutable 一年 + immutable。`immutable` 的含义是"在 max-age 期间
	// 这个 URL 的字节**保证不变**"，于是浏览器连条件请求（If-None-Match）都不发。
	//
	// 敢用它的前提是 URL 本身是内容寻址的：
	//
	//	分片   hls/383/<runID>/1080p/seg_00000.ts  ← runID 每次转码都不同
	//	源片   videos/10/20260915/a3f1c9.mp4      ← 上传时生成的 16 位 hex
	//
	// ⚠ 这个前提是**真的**，不是"应该是"：转码产物写在每次运行唯一的目录里
	// （见 config.HLSRelDir 那段的长注释），一次运行结束之后那个 URL 下的字节
	// 就再也不会被改写。少了那层 runID 目录，同一串分片名会被下一次转码覆盖，
	// 而 immutable 会让浏览器一直用旧的那份 —— 新旧分片混播，**不报错**。
	cacheImmutable = "public, max-age=31536000, immutable"

	// cacheNoCache "存下来，但每次用之前必须回源确认"（304 就不重传）。
	//
	// ⚠ 它不是"不缓存"，很多人在这里写成 `no-store` —— 那会让播放列表
	// **连 304 都拿不到**，每次切档都要重下一份完整清单。no-cache 才是
	// "可以缓存、必须校验"，配合 ETag 的代价是一个 304 的往返。
	cacheNoCache = "no-cache"

	// cacheDay 一天的强缓存。给头像/封面/源片用。
	//
	// 为什么不给它们 immutable：这三样在**同一个 URL 下是可能被换掉的**
	// （重新上传头像到同一个 accountID 目录、封面重做）。分片敢 immutable
	// 正是因为它的 URL 每次运行都是新的，而它们不是。
	cacheDay = "public, max-age=86400"

	// cacheHour 认不出来的扩展名的兜底。
	//
	// 既不是 no-cache 也不是一年：我们**不知道**它是不是内容寻址的，
	// 所以取一个"能省掉大部分重复请求、又不会让改错东西的人等一天"的中间值。
	cacheHour = "public, max-age=3600"
)

// PolicyFor 按扩展名给出响应策略。
//
// 表里**故意没有**通配规则（比如"hls/ 下的都长缓存"）：按路径前缀判断的话，
// 判据就变成了"这个文件在哪"，而缓存策略真正该问的是"这个 URL 下的字节会不会变"。
// 扩展名是后者最接近的可判据，而且它在文件名里，不依赖目录布局。
func PolicyFor(filename string) StaticPolicy {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".m3u8":
		// application/vnd.apple.mpegurl 是 RFC 8216 登记的类型。
		// 有些老教程用 application/x-mpegURL（大写 URL）—— 那是 2014 年前的
		// 事实标准，现代浏览器两者都认，但登记的只有前者。
		return StaticPolicy{ContentType: "application/vnd.apple.mpegurl", CacheControl: cacheNoCache}

	case ".ts":
		// video/mp2t = MPEG-2 Transport Stream。HLS 的分片容器就是它，
		// 即使里面的视频是 H.264（容器和编码是两件事，这个类型名骗人是常事）。
		return StaticPolicy{ContentType: "video/mp2t", CacheControl: cacheImmutable}

	case ".m4s":
		// fMP4 分片（将来的 CMAF 路径）。现在没有生产者，
		// 留在表里是因为**它一旦出现就一定是分片**，而漏掉它的表现是
		// 走兜底的一小时缓存 —— 能跑，只是每次播放多几次 304。
		return StaticPolicy{ContentType: "video/iso.segment", CacheControl: cacheImmutable}

	case ".mp4":
		return StaticPolicy{ContentType: "video/mp4", CacheControl: cacheDay}
	case ".jpg", ".jpeg":
		return StaticPolicy{ContentType: "image/jpeg", CacheControl: cacheDay}
	case ".png":
		return StaticPolicy{ContentType: "image/png", CacheControl: cacheDay}
	case ".gif":
		return StaticPolicy{ContentType: "image/gif", CacheControl: cacheDay}
	case ".webp":
		return StaticPolicy{ContentType: "image/webp", CacheControl: cacheDay}

	default:
		// ContentType 留空 = 交给 ServeContent 嗅探（它读前 512 字节）。
		return StaticPolicy{CacheControl: cacheHour}
	}
}

// staticHandler 挂到 `/static/*filepath` 上的处理器。
//
// 用的是**和 r.Static 同一个根目录来源**（cfg.Storage）—— 这个根目录曾经
// 硬编码在五个地方，收成 config 之后这里也必须走它，否则静态服务和
// 写入侧会分叉（见 config/paths.go 顶部那段）。
func staticHandler(storage config.StorageConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		// gin 的 wildcard 参数**自带前导斜杠**（`/static/hls/1/x.ts` 的
		// filepath 是 `/hls/1/x.ts`），而 config.StaticURLPrefix 以斜杠**结尾**。
		// 直接相加会得到 `/static//hls/1/x.ts` —— 中间那个空段会被 DiskPath
		// 的逐段检查拒掉，于是**每一个静态资源都 404**。
		//
		// 这个 bug 的形状值得记一笔：它不是"某个文件 404"，是"全站静态资源
		// 一起 404"，而前端看到的是白屏 + 控制台一堆 404 —— 一眼就知道是
		// 静态服务坏了。真正危险的是它的反面（只坏一类文件），所以
		// static_test.go 里第一条用例就是普通的 mp4。
		urlPath := config.StaticURLPrefix + strings.TrimPrefix(c.Param("filepath"), "/")

		diskPath, ok := storage.DiskPath(urlPath)
		if !ok {
			// 不记日志：play_url / 图片 URL 由客户端传，错误输入是常态，
			// 每个都记会把真正的告警淹掉（同 probeSource 里的取舍）。
			c.Status(http.StatusNotFound)
			return
		}

		f, err := os.Open(diskPath)
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		defer func() { _ = f.Close() }()

		st, err := f.Stat()
		if err != nil || st.IsDir() {
			// 目录也当 404：这份处理器只服务文件。
			// （列目录是 http.Dir 的默认行为，而 r.Static 用的 FileServer
			// 会去读 index.html —— 两个都不是我们想要的。）
			c.Status(http.StatusNotFound)
			return
		}

		policy := PolicyFor(diskPath)
		if policy.ContentType != "" {
			c.Header("Content-Type", policy.ContentType)
		}
		if policy.CacheControl != "" {
			c.Header("Cache-Control", policy.CacheControl)
		}
		// ETag 取"大小-纳秒时间戳"。不读文件内容算哈希：静态资源里有
		// 45 MB 的视频，为了一次条件请求去哈希整个文件是本末倒置。
		// 弱校验够了 —— 这两项变了，文件基本一定变了。
		c.Header("ETag", fmt.Sprintf(`"%x-%x"`, st.Size(), st.ModTime().UnixNano()))

		// ServeContent 负责：Range（206/416）、If-None-Match / If-Modified-Since（304）、
		// Content-Length、Accept-Ranges。modtime 传真实值，否则 304 永远不成立。
		http.ServeContent(c.Writer, c.Request, st.Name(), st.ModTime(), f)
	}
}

// mountStatic 注册静态资源路由。GET 和 HEAD 都要挂：
//
//	GET   —— 正常取文件
//	HEAD  —— 有些播放器/代理在决定要不要拉之前先探一次（尤其 Safari）
//
// r.Static 内部也是同时挂这两个的，漏掉 HEAD 的话表现是"某些播放器
// 加载媒体失败"，而在 Chrome 里完全复现不出来。
func mountStatic(r *gin.Engine, storage config.StorageConfig) {
	h := staticHandler(storage)
	r.GET("/static/*filepath", h)
	r.HEAD("/static/*filepath", h)
	log.Printf("[static] /static → %s（.m3u8=%s / .ts=immutable / .mp4=%s）",
		storage.Root(), cacheNoCache, cacheDay)
}
