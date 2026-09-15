package config

// paths_test.go —— DiskPath 的**两种形状**和**穿越防护**。
//
// ---------- 为什么这个测试必须存在 ----------
//
// 这个函数是唯一一条"客户端传进来的字符串 → 服务端磁盘路径"的通路。
// 它有两个完全不同的失败方向，而**两个都不报错**：
//
//	太严：绝对 URL 也拒掉 → 门禁对全站每一条视频都"探测失败 → 直传"，
//	      日志是正常的 "probe failed"，功能静默失效（**已经真发生过**）
//	太松：放过去一个 `..` → 任意本地文件可读
//
// 前者是我实测踩到的（上传接口回的是 buildAbsoluteURL 的完整 URL，
// 前端原样回传，所以 PlayPath 里根本没有 `/static/` 开头的），后者是安全底线。
// 两种都只能靠测试钉住。

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDiskPath(t *testing.T) {
	store := StorageConfig{UploadRoot: "./.run/uploads"}
	root := filepath.Clean("./.run/uploads")

	tests := []struct {
		name string
		// in 是客户端传进来的 PlayURL
		in string
		// wantOK false 表示必须拒绝
		wantOK bool
		// wantRel 期望解出来的相对 root 的路径（用 "/" 分隔）
		wantRel string
	}{
		// ---------- 裸路径式（内部调用、老数据）----------
		{name: "裸路径式", in: "/static/videos/1/20260915/a.mp4", wantOK: true, wantRel: "videos/1/20260915/a.mp4"},
		{name: "头像也是同一套", in: "/static/avatars/7/x.png", wantOK: true, wantRel: "avatars/7/x.png"},
		{name: "HLS 分片", in: "/static/hls/375/720p/seg_001.ts", wantOK: true, wantRel: "hls/375/720p/seg_001.ts"},

		// ---------- 绝对 URL 式（**真实链路**：buildAbsoluteURL 的产物）----------
		{
			name:    "绝对 URL 式 —— 真实链路就是这一种",
			in:      "http://127.0.0.1:8080/static/videos/10/20260915/a.mp4",
			wantOK:  true,
			wantRel: "videos/10/20260915/a.mp4",
		},
		{
			name:    "https + 域名 + 端口",
			in:      "https://example.com:443/static/videos/1/a.mp4",
			wantOK:  true,
			wantRel: "videos/1/a.mp4",
		},
		{
			// host 不校验是刻意的：换域名/端口部署时不必同步改配置，
			// 而且它只影响"读我们本地哪个相对路径"，不构成穿越。
			name:    "别的 host 也接受（只取 path）",
			in:      "http://evil.example/static/videos/1/a.mp4",
			wantOK:  true,
			wantRel: "videos/1/a.mp4",
		},

		// ---------- 拒绝：前缀不对 ----------
		{name: "空串", in: "", wantOK: false},
		{name: "只有前缀", in: "/static/", wantOK: false},
		{name: "不是 /static", in: "/uploads/videos/1/a.mp4", wantOK: false},
		{name: "相对路径", in: "videos/1/a.mp4", wantOK: false},
		{name: "http 但 path 不在 /static", in: "http://h/x/videos/1/a.mp4", wantOK: false},

		// ---------- 拒绝：穿越（**这是安全底线**）----------
		{name: "裸路径穿越", in: "/static/../../../Windows/win.ini", wantOK: false},
		{name: "穿越藏在中间", in: "/static/videos/../../../../etc/passwd", wantOK: false},
		{name: "绝对 URL 里穿越", in: "http://h:8080/static/../../../Windows/win.ini", wantOK: false},
		{
			// ⚠ 这一条是选用 u.Path（已解码）而不是 u.EscapedPath() 的理由：
			// 用 EscapedPath 的话 `%2e%2e` 会原样留着，看起来无害。
			name:   "百分号编码的穿越（%2e%2e）",
			in:     "/static/%2e%2e/%2e%2e/Windows/win.ini",
			wantOK: false,
		},
		{
			name:   "编码穿越 + 绝对 URL",
			in:     "http://h:8080/static/%2e%2e/%2e%2e/Windows/win.ini",
			wantOK: false,
		},
		{name: "单个点也要拒", in: "/static/./videos/a.mp4", wantOK: false},
		{name: "空段（重复斜杠）", in: "/static//videos/a.mp4", wantOK: false},
		{name: "尾随斜杠成空段", in: "/static/videos/a.mp4/", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := store.DiskPath(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("DiskPath(%q) ok=%v, want %v（拿到 %q）", tt.in, ok, tt.wantOK, got)
			}
			if !tt.wantOK {
				if got != "" {
					t.Errorf("拒绝时应当返回空串，拿到 %q", got)
				}
				return
			}
			// 解出来的必须是 root 下的那个文件
			want := filepath.Join(root, filepath.FromSlash(tt.wantRel))
			if got != want {
				t.Errorf("DiskPath(%q) = %q, want %q", tt.in, got, want)
			}
			// 再独立验一次"确实落在 root 里" —— 上面是比字符串，这条是比性质。
			// 字符串比对会跟着实现一起改，这条不会。
			if got != root && !strings.HasPrefix(got, root+string(filepath.Separator)) {
				t.Errorf("解出来的路径跳出了 root: %q", got)
			}
		})
	}
}

// TestDiskPathAbsoluteURLEqualsBarePath 把这次翻车的**根因**单独立一条：
// 同一条资源的两种写法必须解出**同一个磁盘路径**。
//
// 这条断言如果断了，说明有人只修了一种形状 —— 而那正是这次 bug 的样子：
// 内部调用（裸路径）一直是好的，真实链路（绝对 URL）一直失效。
func TestDiskPathAbsoluteURLEqualsBarePath(t *testing.T) {
	store := StorageConfig{UploadRoot: "./.run/uploads"}
	bare, ok1 := store.DiskPath("/static/videos/10/20260915/a.mp4")
	abs, ok2 := store.DiskPath("http://127.0.0.1:8080/static/videos/10/20260915/a.mp4")
	if !ok1 || !ok2 {
		t.Fatalf("两种形状都应该解得出来：bare=%v abs=%v", ok1, ok2)
	}
	if bare != abs {
		t.Errorf("同一资源两种写法解出了不同路径：\n  bare = %q\n  abs  = %q", bare, abs)
	}
}

// TestStorageDirsShareOneRoot 钉住"五个写入点同源"这件事。
//
// 这个测试**不关心具体路径**，它关心的是：所有目录都由 Root() 派生。
// 只要 Root() 一改（或 UploadRoot 配错），三个写入目录必须一起动 ——
// 门禁探测读的是同一个 Root()，所以这条保证了它们不会分叉。
func TestStorageDirsShareOneRoot(t *testing.T) {
	store := StorageConfig{UploadRoot: "/tmp/altroot"}
	want := filepath.Clean("/tmp/altroot")

	for _, d := range []struct {
		name string
		got  string
	}{
		{"VideosDir", store.VideosDir(10, "20260915")},
		{"CoversDir", store.CoversDir(10, "20260915")},
		{"AvatarsDir", store.AvatarsDir(10)},
	} {
		if !strings.HasPrefix(filepath.Clean(d.got), want+string(filepath.Separator)) {
			t.Errorf("%s = %q，没有落在 Root()=%q 下", d.name, d.got, want)
		}
	}
}

// TestStorageRootFallsBack 零值必须自愈。
//
// 手写 &StorageConfig{} 的地方（测试、小工具）拿到空 UploadRoot 时，
// filepath.Join("", "videos") = "videos" —— 文件会落到**工作目录**下，
// 不报错、只是找不到了。让访问器兜底比指望每个构造点都调 withDefaults 可靠。
func TestStorageRootFallsBack(t *testing.T) {
	var zero StorageConfig
	if got := zero.Root(); got != DefaultUploadRoot {
		t.Errorf("零值 Root() = %q, want %q", got, DefaultUploadRoot)
	}
	// 比的是 Clean 之后的形式：DefaultUploadRoot 写的是 "./.run/uploads"，
	// 而 Join 出来的是 ".run\uploads\videos\..." —— 直接比字符串会在
	// `./` 前缀和分隔符上假失败（第一次写这条断言就是这么错的）。
	root := filepath.Clean(DefaultUploadRoot)
	if got := zero.VideosDir(1, "20260915"); !strings.HasPrefix(filepath.Clean(got), root+string(filepath.Separator)) {
		t.Errorf("零值的 VideosDir 没有用默认根目录兜底: %q (root=%q)", got, root)
	}
}
