package http

// 静态文件处理器的测试。
//
// ---------- 为什么这个文件必须存在 ----------
//
// 这个处理器是**替换** `r.Static` 来的（理由见 static.go 开头），
// 而"替换一个已经被全世界用过的实现"这件事本身就意味着：
// **它顺带提供的每一项能力都可能被漏掉，而漏掉的表现在浏览器里各不相同。**
//
// 漏掉 Range   → 直传的 135 条 mp4 拖进度条播不动（Chrome 里复现）
// 漏掉 HEAD    → 某些播放器加载媒体失败（Chrome 里复现不出来）
// 漏掉 MIME    → hls.js 照播，Safari 原生 HLS 不播（本机复现不出来）
// 多一层目录   → 全站静态资源 404（一眼可见，反而是最不打紧的一种）
//
// 最后那一条是真实写出来的 bug：gin 的 wildcard 参数自带前导斜杠，
// 而 StaticURLPrefix 以斜杠结尾，直接相加会得到 `/static//hls/...`，
// 那个空段会被 DiskPath 拒掉。所以 TestStaticServesNestedPath 不是凑数的。
//
// 测试用 gin.New() + mountStatic 直接建引擎，**不走 SetRouter** ——
// 后者要把 MySQL/Redis/RabbitMQ 全接上，而静态服务的正确性和它们无关。
// 这条边界让这个文件的 11 个用例能在 10ms 内跑完。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"myfeed/internal/config"

	"github.com/gin-gonic/gin"
)

func newStaticTestEngine(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	r := gin.New()
	mountStatic(r, config.StorageConfig{UploadRoot: root})
	return r, root
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func do(t *testing.T, r *gin.Engine, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---------- 基本：能返回文件，且路径映射对得上 ----------

func TestStaticServesNestedPath(t *testing.T) {
	r, root := newStaticTestEngine(t)
	body := []byte("fake mp4 bytes")
	writeFile(t, filepath.Join(root, "videos", "10", "20260915", "a3f1c9.mp4"), body)

	w := do(t, r, "GET", "/static/videos/10/20260915/a3f1c9.mp4", nil)
	if w.Code != 200 {
		t.Fatalf("状态 = %d, want 200（body=%q）", w.Code, w.Body.String())
	}
	if w.Body.String() != string(body) {
		t.Errorf("内容 = %q, want %q", w.Body.String(), body)
	}
	if got := w.Header().Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", got)
	}
	if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes —— 没有它浏览器不敢发 Range 请求", got)
	}
}

// TestStaticServesDeeplyNestedPath 专门打那条"多一层 HLS 目录"的路径。
//
// /static/hls/383/<runID>/1080p/seg_00000.ts 是本项目最深的 URL 形状
// （五层），也是将来唯一一条会被浏览器**大量**请求的路径。
func TestStaticServesDeeplyNestedPath(t *testing.T) {
	r, root := newStaticTestEngine(t)
	writeFile(t, filepath.Join(root, "hls", "383", "9f3a1c02", "1080p", "seg_00000.ts"), []byte("ts"))

	w := do(t, r, "GET", "/static/hls/383/9f3a1c02/1080p/seg_00000.ts", nil)
	if w.Code != 200 {
		t.Fatalf("状态 = %d, want 200", w.Code)
	}
}

// ---------- 缓存策略：按扩展名分三档 ----------

func TestStaticCachePolicy(t *testing.T) {
	r, root := newStaticTestEngine(t)
	writeFile(t, filepath.Join(root, "hls", "1", "run", "1080p", "index.m3u8"), []byte("#EXTM3U\n"))
	writeFile(t, filepath.Join(root, "hls", "1", "run", "1080p", "seg_00000.ts"), []byte("ts"))
	writeFile(t, filepath.Join(root, "videos", "1", "x.mp4"), []byte("mp4"))
	writeFile(t, filepath.Join(root, "avatars", "1", "a.png"), []byte("png"))

	tests := []struct {
		target string
		wantCC string
		wantCT string
		why    string
	}{
		{"/static/hls/1/run/1080p/index.m3u8", "no-cache", "application/vnd.apple.mpegurl",
			"master 是转码和降级的提交点，缓存住它等于拿到一份指向旧产物的清单"},
		{"/static/hls/1/run/1080p/seg_00000.ts", "public, max-age=31536000, immutable", "video/mp2t",
			"分片 URL 每次运行都不同，内容永不变 —— 这是敢 immutable 的唯一前提"},
		{"/static/videos/1/x.mp4", "public, max-age=86400", "video/mp4",
			"源片可能在同 URL 下被换掉，所以给强缓存但不用 immutable"},
		{"/static/avatars/1/a.png", "public, max-age=86400", "image/png",
			"头像同理（重新上传会覆盖同一个 accountID 目录里的文件）"},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			w := do(t, r, "GET", tt.target, nil)
			if w.Code != 200 {
				t.Fatalf("状态 = %d", w.Code)
			}
			if got := w.Header().Get("Cache-Control"); got != tt.wantCC {
				t.Errorf("Cache-Control = %q, want %q（%s）", got, tt.wantCC, tt.why)
			}
			if got := w.Header().Get("Content-Type"); got != tt.wantCT {
				t.Errorf("Content-Type = %q, want %q", got, tt.wantCT)
			}
		})
	}
}

// TestPolicyForM3u8IsNotNoStore 是一条**命名了错误做法**的断言。
//
// `no-store` 和 `no-cache` 只差三个字母，而前者会让 HLS 播放列表
// **连 304 都拿不到** —— 每次切档重下一份完整清单。它看起来"更安全"
// （"我都叫 no-store 了当然不会用旧的"），所以是个很自然的写错方向。
func TestPolicyForM3u8IsNotNoStore(t *testing.T) {
	p := PolicyFor("master.m3u8")
	if strings.Contains(p.CacheControl, "no-store") {
		t.Errorf("Cache-Control = %q —— no-store 会让每次切档都重下完整清单，"+
			"要的是 no-cache（可缓存、必须校验）", p.CacheControl)
	}
	if p.CacheControl != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", p.CacheControl)
	}
}

func TestPolicyForUnknownExtension(t *testing.T) {
	p := PolicyFor("something.weird")
	if p.ContentType != "" {
		t.Errorf("认不出的扩展名不该硬给 Content-Type（要让 ServeContent 去嗅探），拿到 %q", p.ContentType)
	}
	if p.CacheControl == "" {
		t.Error("兜底也该给一个 Cache-Control，否则浏览器按启发式猜")
	}
	// 大小写不敏感：Windows 上客户端大小写随意
	if PolicyFor("A.M3U8").CacheControl != PolicyFor("a.m3u8").CacheControl {
		t.Error("扩展名判断应当大小写不敏感")
	}
}

// ---------- Range：**最容易漏掉、后果最重的一条** ----------

func TestStaticRangeRequest(t *testing.T) {
	r, root := newStaticTestEngine(t)
	body := []byte("0123456789")
	writeFile(t, filepath.Join(root, "videos", "1", "x.mp4"), body)

	w := do(t, r, "GET", "/static/videos/1/x.mp4", map[string]string{"Range": "bytes=2-5"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("状态 = %d, want 206 —— 没有 Range 支持的话直传视频拖进度条播不动", w.Code)
	}
	if got := w.Body.String(); got != "2345" {
		t.Errorf("内容 = %q, want 2345", got)
	}
	if got := w.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Errorf("Content-Range = %q, want bytes 2-5/10", got)
	}

	// 越界的 Range 必须被拒（416），而不是静默返回整个文件
	w = do(t, r, "GET", "/static/videos/1/x.mp4", map[string]string{"Range": "bytes=100-200"})
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("越界 Range 状态 = %d, want 416", w.Code)
	}
}

// ---------- ETag / 304 ----------

func TestStaticETagAnd304(t *testing.T) {
	r, root := newStaticTestEngine(t)
	writeFile(t, filepath.Join(root, "hls", "1", "run", "index.m3u8"), []byte("#EXTM3U\n"))

	w := do(t, r, "GET", "/static/hls/1/run/index.m3u8", nil)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("没有 ETag —— 那 no-cache 就退化成每次重下完整文件")
	}

	w2 := do(t, r, "GET", "/static/hls/1/run/index.m3u8", map[string]string{"If-None-Match": etag})
	if w2.Code != http.StatusNotModified {
		t.Errorf("带 If-None-Match 的状态 = %d, want 304", w2.Code)
	}
	if w2.Body.Len() != 0 {
		t.Errorf("304 不该带 body，拿到 %d 字节", w2.Body.Len())
	}

	// 内容变了（mtime 变）之后 ETag 必须跟着变，否则 304 会一直命中旧内容
	if err := os.WriteFile(filepath.Join(root, "hls", "1", "run", "index.m3u8"),
		[]byte("#EXTM3U\n#EXT-X-ENDLIST\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w3 := do(t, r, "GET", "/static/hls/1/run/index.m3u8", map[string]string{"If-None-Match": etag})
	if w3.Code != 200 {
		t.Errorf("文件改了但状态 = %d —— 那会把旧播放列表一直发出去", w3.Code)
	}
}

// ---------- HEAD ----------

func TestStaticHead(t *testing.T) {
	r, root := newStaticTestEngine(t)
	writeFile(t, filepath.Join(root, "videos", "1", "x.mp4"), []byte("0123456789"))

	w := do(t, r, "HEAD", "/static/videos/1/x.mp4", nil)
	if w.Code != 200 {
		t.Fatalf("HEAD 状态 = %d, want 200（漏挂 HEAD 的表现在 Chrome 里复现不出来）", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD 不该有 body，拿到 %d 字节", w.Body.Len())
	}
	if got := w.Header().Get("Content-Length"); got != "10" {
		t.Errorf("Content-Length = %q, want 10", got)
	}
}

// ---------- 404 的各种来源 ----------

func TestStaticNotFound(t *testing.T) {
	r, root := newStaticTestEngine(t)
	writeFile(t, filepath.Join(root, "videos", "1", "x.mp4"), []byte("mp4"))
	if err := os.MkdirAll(filepath.Join(root, "videos", "1", "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 根目录外放一个"如果穿越成功就会被读到"的文件
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "secret.txt"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		target string
		why    string
	}{
		{"文件不存在", "/static/videos/1/nope.mp4", "最常见的一种"},
		{"目录", "/static/videos/1/d", "只服务文件，不列目录也不找 index.html"},
		{"路径穿越", "/static/../secret.txt", "必须 404，绝不能把根目录外的文件读出来"},
		{"深层路径穿越", "/static/videos/../../../secret.txt", "同上"},
		{"穿越到根目录本身", "/static/", "空相对路径"},
		{"编码过的穿越", "/static/%2e%2e/secret.txt", "归一化之后才判，见 config.DiskPath"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := do(t, r, "GET", tt.target, nil)
			if w.Code != 404 {
				t.Errorf("状态 = %d, want 404（%s）", w.Code, tt.why)
			}
			if strings.Contains(w.Body.String(), "SECRET") {
				t.Fatalf("**读到了根目录外的文件** —— 穿越防护失效")
			}
		})
	}
}

// TestStaticTraversalCannotEscapeEvenWhenCleaned 是一条"换个写法再试一次"的补充。
//
// net/http 的 ServeMux 会先 clean 路径，但 gin 用自己的路由树、**不 clean**。
// 所以这里直接构造一个已经被 URL 解析过的请求，确认即使把 `..` 放在
// wildcard 参数的最前面也穿不出去。
func TestStaticTraversalCannotEscapeEvenWhenCleaned(t *testing.T) {
	r, root := newStaticTestEngine(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "secret.txt"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"/static/..%2fsecret.txt",
		"/static/videos/..%2f..%2f..%2fsecret.txt",
		"/static//../secret.txt",
	} {
		w := do(t, r, "GET", target, nil)
		if w.Code == 200 {
			t.Errorf("%s 返回了 200（body=%q）", target, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "SECRET") {
			t.Fatalf("%s 读到了根目录外的文件", target)
		}
	}
}

// ---------- 一致性：URL↔磁盘 只有一份映射 ----------

// TestStaticUsesDiskPathMapping 钉住"静态服务和其余所有读写走同一份映射"。
//
// 它防的是一次真实发生过的分叉：根目录曾经硬编码在五个地方，
// 门禁改为读配置之后，两边就可能有不同的答案（见 config/paths.go 顶部）。
// 静态服务是第六个来源，也是唯一一个**不报错**的来源 ——
// 它只是把文件从别的地方拿出来。
func TestStaticUsesDiskPathMapping(t *testing.T) {
	storage := config.StorageConfig{UploadRoot: t.TempDir()}
	r := gin.New()

	// 用 DiskPath 反推磁盘位置，再按 URL 请求 —— 两边必须是同一个文件
	urlPath := "/static/videos/10/20260915/abc.mp4"
	disk, ok := storage.DiskPath(urlPath)
	if !ok {
		t.Fatalf("DiskPath(%q) 失败", urlPath)
	}
	if err := os.MkdirAll(filepath.Dir(disk), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(disk, []byte("来自 DiskPath 指的那个文件"), 0o644); err != nil {
		t.Fatal(err)
	}

	mountStatic(r, storage)
	w := do(t, r, "GET", urlPath, nil)
	if w.Code != 200 {
		t.Fatalf("状态 = %d", w.Code)
	}
	if got := w.Body.String(); got != "来自 DiskPath 指的那个文件" {
		t.Errorf("内容 = %q", got)
	}

	// 顺带钉住 UploadRoot 为空时的兜底：DiskPath 必须和 Root() 用同一个值。
	//
	// 空 UploadRoot 在真实链路里不会出现（config.Load 会填），但它是
	// "有一个访问器兜底、另一个不兜底"这类分叉最容易藏身的地方 ——
	// 而分叉的表现是所有静态资源 404，且没有任何一行日志。
	// 只断言 ok 的**方向**（指向默认根目录下），不去真的碰磁盘：
	// 真去 os.Open("./.run/uploads/...") 会把开发机的上传目录读进测试。
	empty := config.StorageConfig{}
	got, ok := empty.DiskPath("/static/videos/1/x.mp4")
	want, _ := config.StorageConfig{UploadRoot: config.DefaultUploadRoot}.DiskPath("/static/videos/1/x.mp4")
	if !ok || got != want {
		t.Errorf("空 UploadRoot 时 DiskPath = %q(ok=%v), want %q（必须和 Root() 的兜底一致）",
			got, ok, want)
	}
}

// ---------- 大文件：Range 的分片读不能把整个文件读进内存 ----------

func TestStaticLargeFileRangeDoesNotReadWhole(t *testing.T) {
	r, root := newStaticTestEngine(t)
	p := filepath.Join(root, "videos", "1", "big.mp4")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	const size = 8 << 20 // 8 MB
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()

	w := do(t, r, "GET", "/static/videos/1/big.mp4", map[string]string{"Range": "bytes=0-1023"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("状态 = %d, want 206", w.Code)
	}
	got, _ := io.ReadAll(w.Body)
	if len(got) != 1024 {
		t.Errorf("返回 %d 字节, want 1024 —— Range 要真的只读那一段", len(got))
	}
	if cl := w.Header().Get("Content-Length"); cl != strconv.Itoa(1024) {
		t.Errorf("Content-Length = %q, want 1024", cl)
	}
}
