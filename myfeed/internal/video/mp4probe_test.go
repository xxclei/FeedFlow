package video

// mp4 探针的测试。**两个 fixture 的存在理由是这个文件的核心。**
//
// ---------- 为什么必须有两个，而不是一个 ----------
//
// MP4 的 `moov`（装着时长和宽高的那个 box）可以出现在文件**任意位置**：
//
//	moov 在头（faststart）  ftyp moov free mdat   ← 网页优化的典型布局
//	moov 在尾（默认）       ftyp free mdat moov   ← 手机原片/未优化的典型布局
//
// 存量 135 个素材**全部是 faststart**（实测：`moov` 全在文件头，见
// scripts/mp4_probe.py 的注释）。也就是说只用真素材验证的话，
// "moov 在尾"这条路径**从来没被走过** —— 而它恰好是有人直接传手机原片时的布局，
// 也就是这个门禁最想拦的那类素材。
//
// 所以这两个 fixture 是**刻意造出来补覆盖**的，不是从素材里抄的：
// 它们同尺寸、同时长、连字节数都一样（24064），唯一的区别就是 box 顺序。
// 这样"两个都解得出来且结果相同"这句话就只可能归因于解析器真的按 box 链
// 去 seek 了，而不是靠"文件长得巧"。
//
// 生成命令（ffmpeg 9.0.1，装在 .run/bin/）：
//
//	ffmpeg -f lavfi -i testsrc2=size=320x240:rate=10 -t 1 -c:v libx264 \
//	       -b:v 200k -pix_fmt yuv420p                    testdata/moov_at_end.mp4
//	ffmpeg ... 同上 ... -movflags +faststart            testdata/faststart.mp4
//
// 期望值全部来自 **ffprobe 的独立输出**（不是来自本解析器的输出）：
//
//	width=320 height=240 duration=1.000000 size=24064 bit_rate=192512
//
// ⚠ bit_rate 192512 **bps** = 192.512 kbps，而本解析器存的是 kbps 整数，
// 所以断言 192（截断）而不是 193。这个"谁截断谁进位"的差别只值得一句注释：
// 1 kbps 的误差对门禁的判定毫无影响（阈值差着上千 kbps），
// 但**把 ffprobe 的 bps 当成 kbps 写进断言**会差 1000 倍，那是必须防的。

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	fixtureFaststart = "testdata/faststart.mp4"
	fixtureMoovAtEnd = "testdata/moov_at_end.mp4"

	// 两个 fixture 由同一条命令产出（只差 -movflags），所以这些值共用
	fixtureSize   int64 = 24064
	fixtureWidth        = 320
	fixtureHeight       = 240
	fixtureDurMS  int64 = 1000
	fixtureKbps         = 192 // = 24064*8/1000 截断（ffprobe 口径 192.512 kbps）
)

func TestProbeMP4BothLayouts(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
		// why 记的是"这个布局在真实世界里长什么样"
		why string
	}{
		{"moov 在尾（手机原片）", fixtureMoovAtEnd, "未优化的相机输出，正是门禁最想拦的那类"},
		{"moov 在头（faststart）", fixtureFaststart, "网页优化过的，和存量 135 个素材一样"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info, err := ProbeMP4(tt.path)
			if err != nil {
				t.Fatalf("ProbeMP4(%s) 失败: %v（%s）", tt.path, err, tt.why)
			}
			if info.Width != fixtureWidth || info.Height != fixtureHeight {
				t.Errorf("尺寸 = %dx%d, want %dx%d", info.Width, info.Height, fixtureWidth, fixtureHeight)
			}
			if info.DurationMS != fixtureDurMS {
				t.Errorf("DurationMS = %d, want %d", info.DurationMS, fixtureDurMS)
			}
			if info.SizeBytes != fixtureSize {
				t.Errorf("SizeBytes = %d, want %d", info.SizeBytes, fixtureSize)
			}
			if info.BitrateKbps != fixtureKbps {
				t.Errorf("BitrateKbps = %d, want %d（ffprobe 是 192512 bps，别把 bps 当 kbps）",
					info.BitrateKbps, fixtureKbps)
			}
		})
	}
}

// TestProbeMP4LayoutsAgree 把上面那条"两个 fixture 只有 box 顺序不同"
// 变成一个**可执行的断言**，而不是注释里的一句声称。
//
// 它防的是：哪天有人"顺手"重新生成了一个 fixture（比如换了 ffmpeg 参数），
// 两个文件不再对称，于是上面那个测试仍然全绿，但它想证明的东西已经没了。
func TestProbeMP4LayoutsAgree(t *testing.T) {
	a, err := ProbeMP4(fixtureFaststart)
	if err != nil {
		t.Fatalf("faststart: %v", err)
	}
	b, err := ProbeMP4(fixtureMoovAtEnd)
	if err != nil {
		t.Fatalf("moov_at_end: %v", err)
	}
	if *a != *b {
		t.Errorf("两个 fixture 应当解出完全相同的 MP4Info（它们只差 box 顺序）:\n  faststart  = %+v\n  moov_at_end= %+v", *a, *b)
	}
	if a.SizeBytes != b.SizeBytes {
		t.Errorf("两个 fixture 字节数应当相同，否则上面那句'只差 box 顺序'不成立: %d vs %d",
			a.SizeBytes, b.SizeBytes)
	}
}

// ---------- 失败路径：**一律返回 error，绝不 panic** ----------
//
// 这不是洁癖：probeSource 在 Publish 的事务**之前**跑，一个 panic 会
// 把整个发布流程带走。而它的输入是用户上传的文件，什么形状都可能有。

func TestProbeMP4RejectsNonMP4(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name string
		// write 造文件内容；返回要写入的字节
		write []byte
	}{
		{"空文件", nil},
		{"纯文本", []byte("this is definitely not an mp4 file, just some words")},
		{"只有 ftyp 头", []byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00isomiso2")},
		{
			// 声明了一个巨大的 box，但文件只有几个字节 —— 截断的典型形态。
			// 解析器必须靠"size 超出文件末尾就放弃"挡住，而不是先分配再读。
			"声称的长度超出文件实际大小",
			[]byte("\x00\x00\xff\xffmoov\x00\x00\x00\x10mvhd"),
		},
		{"全零 1KB", make([]byte, 1024)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(dir, "x.mp4")
			if err := os.WriteFile(p, tt.write, 0o644); err != nil {
				t.Fatal(err)
			}
			info, err := ProbeMP4(p)
			if err == nil {
				t.Fatalf("应当返回 error，却拿到了 %+v", info)
			}
			// 失败时**不能**返回半成品 info：调用方会拿它去填 src_width。
			// 返回 nil 而不是零值 struct，是为了让"忘了判 err"直接 panic，
			// 而不是静默写入全 0 的一行。
			if info != nil {
				t.Errorf("失败时应当返回 nil info（避免调用方静默写全 0），拿到 %+v", info)
			}
		})
	}
}

// TestProbeMP4MissingFile 文件不存在是**最常见**的一种失败
// （上传和发布是解耦的，理论上文件应该已经在，但"理论上"值一次 stat）。
func TestProbeMP4MissingFile(t *testing.T) {
	info, err := ProbeMP4(filepath.Join(t.TempDir(), "nope.mp4"))
	if err == nil {
		t.Fatalf("文件不存在应当返回 error，拿到 %+v", info)
	}
	if info != nil {
		t.Errorf("失败时应当返回 nil info，拿到 %+v", info)
	}
}

// TestProbeMP4ShortSideBucketing 顺带钉住"竖屏按短边归档"这条**贯穿整条链路**的
// 约定在探针这一层是成立的：探针只报真实的宽高，归档的判断在
// transcode_policy 里做（那边有 targetForShortSide 的测试）。
//
// 这条断言看起来是同义反复，但它防的是一次真实存在的错误倾向：
// 图省事让探针直接返回"档位名"，那会让竖屏视频在这里就被归档错，
// 而且错得不明显。
func TestProbeMP4ReportsRealDimensions(t *testing.T) {
	info, err := ProbeMP4(fixtureMoovAtEnd)
	if err != nil {
		t.Fatal(err)
	}
	if shortSide(info.Width, info.Height) != fixtureHeight {
		t.Errorf("短边 = %d, want %d（横向素材的短边是 height）",
			shortSide(info.Width, info.Height), fixtureHeight)
	}
}
