// Package transcode 把「一条源 mp4 + 一张档位表」变成「一棵 HLS 产物目录」。
//
// ---------- 为什么单独一个包 ----------
//
// 三件事被刻意分开：
//
//	internal/video/transcode_policy.go   **要不要转**（纯判定，无 IO）
//	internal/transcode/（本包）          **怎么转**（拼 ffmpeg 参数、写 playlist）
//	internal/worker/transcode_worker.go  **什么时候转**（MQ、状态机、幂等）
//
// 把它们塞进一个文件当然也能跑。分开是因为三者的**测试方式完全不同**：
// 判定是纯函数（可以穷举），参数拼接是纯函数（可以断言字符串），
// 而状态机必须靠真实 IO 和真实 MQ 才能验。混在一起的话，
// 一个需要跑 ffmpeg 的测试会把纯函数那部分的测试也拖成分钟级。
//
// ---------- 和 internal/video 的依赖方向 ----------
//
// 本包 import video（为了 video.Tier），**反过来不成立**。
// 这是必须的：档位表有三个读者（转码、过载降级、门禁判定），
// 见 Tier 那段注释 —— 三处各定义一份的话，会出现"降级按 4500 算、
// 转码按 3000 生成"这种各算各的，而且不报错。
package transcode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"myfeed/internal/video"
)

// SegmentSeconds HLS 分片时长。
//
// 6 秒是工业界的默认值（Apple 的推荐是 6，Netflix 用 2~4 追求更低延迟）。
// 选 6 而不是 2 的理由是**和这条 12 Mbps 上行匹配**：
//
//	分片越小 → 首帧越快（少等一个分片）→ 但请求数越多、每个请求的开销占比越大
//	分片越大 → 每档的 .ts 越大，切档时需要缓冲的"跨档对齐"越多
//
// 在"总共只能撑几个观众"这个量级上，请求数不是瓶颈，所以偏大一点更省。
// 它对**过载降级**还有个副作用：一次拉取占用带宽的时间更长，
// 并发码率的统计更平滑（不会因为分片太碎而抖动）。
const SegmentSeconds = 6

// Preset x264 的编码档位。
//
// veryfast 是刻意的选择，不是默认值：转码是**离线任务**，慢一点没关系，
// 但这条链路上还有个隐含约束 —— 转码期间 CPU 被吃满会拖慢
// **API 进程**（同一台机器）。medium 的画质提升在这个项目的体量上
// 换不回"转码时页面变卡"的代价。
const Preset = "veryfast"

// Job 一档输出的完整描述：**参数算完之后，执行只是照着它跑**。
//
// Args 里的东西全部来自 Tiers 和源尺寸 —— 也就是说这个结构体是
// "拼参数"这一步的**产出**，而不是它的输入。这样它的单测不需要 ffmpeg：
// 断言 Args 的内容就够了。
type Job struct {
	// Tier 这一档来自档位表（分辨率短边 + 目标码率）
	Tier video.Tier
	// Dir 这一档的产物目录。分片和 index.m3u8 都写在这里面。
	Dir string
	// OutW / OutH 这一档**实际输出**的画面尺寸（不是档位名义上的 1080p）。
	//
	// 为什么需要它：档位表里的 Height 是**短边**，竖屏视频的 1080p 档
	// 输出是 1080×1920 —— 只看档位名分不出宽高，而 master 的
	// RESOLUTION 属性必须写真实的宽×高。
	OutW int
	OutH int
	// Args 喂给 ffmpeg 的参数（不含 argv[0]）。
	Args []string
}

// Plan 算出这次转码要跑的全部 ffmpeg 命令。
//
// tiers 由调用方从档位表里筛（`usableTiers`，只看源素材够得着的那几档）——
// 本函数**不做筛选**：它拿到什么就生成什么。理由是"源够不够得着某一档"
// 是一条业务规则（不能把 360p 拉到 480p），而它已经有一个作者了；
// 再在这里判一次就是第二份口径，两边不一致时是静默放大。
func Plan(source string, srcW, srcH int, runDir string, tiers []video.Tier) []Job {
	jobs := make([]Job, 0, len(tiers))
	for _, t := range tiers {
		w, h := OutputSize(t, srcW, srcH)
		dir := filepath.Join(runDir, t.Name)
		jobs = append(jobs, Job{
			Tier: t,
			Dir:  dir,
			OutW: w,
			OutH: h,
			Args: buildArgs(source, t, dir, w, h, srcW, srcH),
		})
	}
	return jobs
}

// OutputSize 算一档的**实际输出**尺寸。
//
// 规则是"短边对齐档位、长边按源比例缩放、取偶数"。为什么是短边对齐：
// Tier.Height 的定义就是短边（见它的注释），竖屏 1080×1920 属于 1080p 档，
// 它的短边是 1080，长边 1920 —— 缩放时必须锁短边。
//
// ---------- 取偶数是硬约束，不是美观 ----------
//
// H.264 的 yuv420p 对亮度做 2×2 下采样，宽高必须是偶数。
// 奇数会给出一帧警告然后**悄悄改成奇数-1**（或直接失败，看版本）——
// 前者更糟：输出的实际尺寸和 master 里写的 RESOLUTION 对不上，
// 而两边都对不上却都不报错。
//
// 取整方式对齐 ffmpeg 的 `-2`：**先截断，再向上取到偶数**（FFALIGN(v,2)）。
// 实测例子：1280×720 的源 → 480p 档 → 1280×480/720 = 853.33 → 854。
// 手写"四舍五入到偶数"会在这种 .33 的边界上差 2 像素，
// 而差 2 像素不会让任何东西失败 —— 只会让 RESOLUTION 是个假数。
//
// ⚠ **诚实边界**：RESOLUTION 是**推算**出来的，不是从产物里读出来的。
// 它和 ffmpeg 的 `-2` 在数学上一致（见上），但"一致"的证明是这段推导，
// 不是一句断言。验收时用 ffprobe 对一次就够 —— 见 scripts/bench-notes-transcode.md。
func OutputSize(t video.Tier, srcW, srcH int) (int, int) {
	if srcW <= 0 || srcH <= 0 {
		// 源尺寸不知道（探测失败）。回落到档位名义值，但**这不影响任何东西**：
		// 拿不到源尺寸时 worker 根本走不到这里（它会先判失败），
		// 这个分支存在的意义只是别让 OutputSize 在坏输入上 panic。
		return evenAlign(t.Height * 16 / 9), t.Height
	}
	if srcW >= srcH {
		// 横屏：锁高（短边 = 高）
		return evenAlign(srcW * t.Height / srcH), t.Height
	}
	// 竖屏：锁宽（短边 = 宽）
	return t.Height, evenAlign(srcH * t.Height / srcW)
}

// evenAlign 截断后向上取到偶数，和 ffmpeg `-2` 的 FFALIGN(v,2) 同口径。
func evenAlign(v int) int {
	if v < 2 {
		return 2
	}
	return (v + 1) &^ 1
}

// ScaleFilter 一档的 `-vf` 表达式。
//
// 两件事，第二件容易被漏掉：
//
//  1. **scale** —— 只在真的需要缩放时加。输出尺寸等于源尺寸时整个跳过，
//     因为 `scale=1920:1080` 在 1920×1080 的源上不是空操作：
//     它会走一遍 swscale（哪怕只是拷贝），白烧一次全帧内存带宽。
//  2. **setsar=1** —— 把像素宽高比归一成 1:1。**这一条不能省。**
//     有些源（手机录屏、DVD 转出来的片子）带非方形像素（比如 SAR 16:15），
//     此时"存储尺寸"和"显示尺寸"不一样：存的是 720×576，显示成 1024×576。
//     HLS 是按方形像素定义的，SAR≠1 的产物在播放器里会被二次拉伸 ——
//     表现是"转码完画面被压扁了"，而 master 里的 RESOLUTION 写的是存储尺寸，
//     看起来完全正常。setsar=1 让存储尺寸就等于显示尺寸，
//     于是 RESOLUTION 这个数才是有意义的。
func ScaleFilter(j Job, srcW, srcH int) string {
	parts := make([]string, 0, 2)
	if srcW > 0 && srcH > 0 && (j.OutW != srcW || j.OutH != srcH) {
		if srcW >= srcH {
			// -2 = 保持比例、取偶数。横屏锁高（短边是高）。
			parts = append(parts, fmt.Sprintf("scale=-2:%d", j.OutH))
		} else {
			parts = append(parts, fmt.Sprintf("scale=%d:-2", j.OutW))
		}
	}
	parts = append(parts, "setsar=1")
	return strings.Join(parts, ",")
}

// buildArgs 拼一条 ffmpeg 命令。
//
// ---------- 每个参数都在防一件具体的事 ----------
//
//	-hide_banner      日志里少 20 行版本信息，剩下的才是要看的
//	-nostdin          **worker 里必须有**。不加的话 ffmpeg 会尝试读 stdin，
//	                  在后台进程里 stdin 可能指向一个永不结束的管道 ——
//	                  表现是 ffmpeg 卡在第一帧不动，而日志一切正常
//	-y                产物目录是新的，正常不会撞；撞上时"覆盖"比"报错退出"好，
//	                  因为重投本来就是要重跑一遍
//	-map 0:v:0        显式取第一路视频。不加的话 ffmpeg 会按"最优"挑，
//	                  有些源里"最优"是内嵌封面图（一路 mjpeg）——
//	                  转出来是一条 1 帧的视频，且不报错
//	-map 0:a:0?       末尾的 `?` = 没有音轨也不报错。静音视频是常见素材，
//	                  少了这个问号它们会全部转码失败
//	-pix_fmt yuv420p  兼容性底线。源是 yuv444p/yuvj420p 时播放器放不出来
//	-profile:v main   最大兼容档（baseline 不支持 B 帧，high 在老设备上有坑）
//	-preset veryfast  见 Preset 的注释
//	-b:v / -maxrate / -bufsize
//	                  **ABR 而不是 CRF**。这是整个扩展的地基：
//	                  过载降级按 `Tier.TotalKbps()` 核算负载，如果实际码率
//	                  由 CRF 决定（随画面复杂度飘），那个核算就是假的 ——
//	                  而"降级算出来还剩 2000 kbps"和"实际拉了 5000 kbps"
//	                  之间没有任何机制会发现对不上。所以宁可画质差一点，
//	                  也要码率可预测。bufsize 取 2 倍是 x264 的常规配比。
//	-force_key_frames 强制每 6 秒一个关键帧，和 SegmentSeconds 对齐。
//	                  不加的话分片边界由场景切换决定，会出现 1.2 秒、
//	                  0.4 秒这种碎片分片（每片一次 HTTP 请求）。
//	-hls_playlist_type vod
//	                  写 `#EXT-X-PLAYLIST-TYPE:VOD` 和 `#EXT-X-ENDLIST`。
//	                  后者让播放器知道"没有更多分片了"（能算出总时长），
//	                  前者让中间缓存敢缓存这个 playlist
//	-hls_flags independent_segments
//	                  声明"每片都能独立解码"。这是切档时不清空缓冲的前提
//	                  （切档只能发生在关键帧上，声明了它播放器才敢直接切）
func buildArgs(source string, t video.Tier, dir string, outW, outH, srcW, srcH int) []string {
	filter := ScaleFilter(Job{OutW: outW, OutH: outH}, srcW, srcH)

	return []string{
		"-hide_banner",
		"-nostdin",
		"-y",
		"-loglevel", "warning",
		"-i", source,
		"-map", "0:v:0",
		"-map", "0:a:0?",
		"-vf", filter,
		"-c:v", "libx264",
		"-preset", Preset,
		"-profile:v", "main",
		"-pix_fmt", "yuv420p",
		"-b:v", kbps(t.VideoKbps),
		"-maxrate", kbps(t.VideoKbps),
		"-bufsize", kbps(t.VideoKbps * 2),
		"-c:a", "aac",
		"-b:a", kbps(t.AudioKbps),
		"-ac", "2",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", SegmentSeconds),
		"-hls_time", strconv.Itoa(SegmentSeconds),
		"-hls_playlist_type", "vod",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(dir, "seg_%05d.ts"),
		filepath.Join(dir, "index.m3u8"),
	}
}

func kbps(v int) string { return strconv.Itoa(v) + "k" }

// Run 跑一条 ffmpeg 命令。
//
// ---------- 用 CommandContext 而不是 Command ----------
//
// 这是"转码**不做优雅等待**"那条决策的落点。worker 收到 Ctrl+C 时，
// 外面的 ctx 被取消，CommandContext 会**杀掉 ffmpeg 子进程**。
// 用普通 Command 的话，主进程退出了 ffmpeg 还在跑（Windows 上孤儿进程
// 不会被回收），留下一个吃满 CPU 的幽灵进程 —— 而且它还在往一个
// 没人会 Ack 的任务的产物目录里写东西。
//
// 杀掉之后这条消息不会被 Ack，所以 MQ 会重投，重跑时走的是新的 runID
// （见 config/paths.go 那段），半截产物不会被任何人引用。
func Run(ctx context.Context, ffmpegPath string, args []string) error {
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)

	// stderr 单独收着。ffmpeg 的 stdout 是给管道复用的二进制数据，
	// 这里不读它，stderr 才是人看的部分。
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	started := time.Now()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg 失败（耗时 %v）: %w；末尾输出:\n%s",
			time.Since(started).Round(time.Millisecond), err, tail(stderr.String(), 800))
	}
	return nil
}

// tail 取字符串末尾 n 个字节。
//
// 为什么取**末尾**而不是开头：ffmpeg 的报错永远在最后几行
// （"Invalid data found"、"Conversion failed"），前面全是进度噪声。
// 开头 800 字节能拿到的只有版本号和输入信息 —— 恰好是最没用的部分。
//
// 按字节切可能把多字节字符切一半（中文日志）。这只影响日志可读性的
// 最后一两个字符，不值得为它引入 rune 解码 —— 但要知道会发生。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// NewRunID 生成一次转码运行的 ID（8 字节 hex = 16 字符）。
//
// 它只要求"同一台机器上两次运行不撞"，所以随机数不需要是密码学强度 ——
// 但用 crypto/rand 的代价和 math/rand 一样（都是 16 字节），
// 而在 Windows 上 math/rand 的全局源初始化有锁竞争，crypto/rand 没有。
//
// ⚠ 降级路径：rand 读失败时回落到纳秒时间戳。这不是"更弱但可接受"，
// 而是"宁可撞也不要让转码因为一个随机数失败"——撞了的后果是
// 两次运行的产物目录重合（前一次已成功的产物可能被覆盖）。
// 实测量级：这个分支在 Windows 上不会走到（crypto/rand 走的是
// RtlGenRandom，没有失败路径），留着是为了"函数不返回错误"这个签名。
func NewRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}
