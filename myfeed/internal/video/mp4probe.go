package video

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
)

// 这个文件是 scripts/mp4_probe.py 的 Go 版 —— 那份 Python 是原型，
// 当时是为了量 111 条素材的真实码率（机器上没有 ffprobe）。
//
// ---------- 为什么不用 ffprobe ----------
//
// 阶段 C 之后项目**确实**会带上 ffprobe（`.run/bin/`），但这里仍然手写解析：
//
//  1. **它在请求路径上。** ProbeMP4 在 VideoService.Publish 里被调用 ——
//     spawn 一个进程是 fork/exec，30~80ms 起步；而这里只读几个文件头，约 1ms。
//     把一个进程启动塞进用户点击"发布"后的等待里，是不可接受的。
//  2. **ffprobe 的输出格式跨版本会变。** 解析它的 JSON 是另一种脆弱，
//     而且坏了之后的表现是"探测字段全空"，不报错。
//  3. **门禁只需要三个数**（时长、宽高、文件大小），它们全在文件头里。
//  4. **降级。** ffmpeg 没装好时，门禁依然能做出判断 —— 只有转码 worker 需要它。
//
// ---------- 它不会读整个文件 ----------
//
// 只读 box 头（8 或 16 字节）+ mvhd/tkhd 的那几十字节。
// 标称 9.4 GB 的素材目录，全量探测是秒级的。
//
// ⚠ 注意**即使 moov 在文件尾**（非 faststart）也能正确解析 ——
// 顶层 box 链是 `seek` 过去的，不是读过去的。所以探测失败**真的**意味着
// 文件有异常（截断/损坏/根本不是 mp4），而不是"布局不常见"。这一点是
// decideTranscode 那边"探测失败就当异常处理"这个决定的前提。

// MP4Info 一次探测的结果。零值代表"没探到"，配合 error 判断。
type MP4Info struct {
	// DurationMS 时长（毫秒）。来自 moov→mvhd 的 duration/timescale。
	DurationMS int64
	// Width / Height 画面像素尺寸。来自**视频那条** trak→tkhd 的 16.16 定点数。
	Width  int
	Height int
	// SizeBytes 文件字节数。码率算不出来时它是唯一能反映"这条素材多大"的量。
	SizeBytes int64
	// BitrateKbps 整体码率 = 文件大小 × 8 / 时长。**包含音轨**。
	//
	// 这是"平均码率"，不是视频轨道的瞬时峰值 —— 对我们够用，因为要回答的
	// 问题只是"这条素材是不是太肥了"。真正精确的逐轨码率要遍历 stsz/stco，
	// 那是另一个量级的代码量，而收益是……更精确地判断同一个问题。
	BitrateKbps int
}

var errBoxNotFound = errors.New("box not found")

// boxHeader 一个 box 的头部信息。off 是 box 起点，payloadOff 是内容起点。
type boxHeader struct {
	typ        string
	off        int64 // box 起点（绝对偏移）
	end        int64 // box 终点（绝对偏移，= off + size）
	payloadOff int64 // 内容起点（绝对偏移）
}

func (b *boxHeader) payloadLen() int64 { return b.end - b.payloadOff }

// nextBox 从 off 开始读一个 box 头。end 是**所在容器**的边界（不是文件大小）——
// 这个区别很重要：moov 内部的 box 链必须以 moov 的终点为界，
// 否则会读到 moov 之后的顶层 box 上去（那些字节碰巧长得像 tkhd 的话就静默出错）。
func nextBox(f io.ReadSeeker, off, end int64) (*boxHeader, error) {
	if off+8 > end {
		return nil, io.EOF
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}

	var hdr [8]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, err
	}

	size := int64(binary.BigEndian.Uint32(hdr[0:4]))
	typ := string(hdr[4:8])
	payloadOff := off + 8

	// size 的两个特殊值。和 mp4_probe.py 一样：
	//   1 = 真实长度在后面 8 字节里（64 位）
	//   0 = 这个 box 一直到容器末尾（只应出现在最后一个 box）
	switch size {
	case 1:
		var big [8]byte
		if _, err := io.ReadFull(f, big[:]); err != nil {
			return nil, err
		}
		size = int64(binary.BigEndian.Uint64(big[:]))
		payloadOff = off + 16
	case 0:
		size = end - off
	}

	// 越界/自相矛盾的 box 一律当解析失败。
	// **这个检查是防死循环的关键**：size 若为 0 或负数，下面的 `off += b.size`
	// 就原地打转，整个循环卡在同一个 box 上转到天荒地老。
	// 文件是我们自己收的，但不能假设它一定规矩。
	if size < payloadOff-off || off+size > end {
		return nil, fmt.Errorf("box %q 尺寸异常: size=%d off=%d end=%d", typ, size, off, end)
	}
	return &boxHeader{typ: typ, off: off, end: off + size, payloadOff: payloadOff}, nil
}

// findBox 在 [start,end) 里找第一个类型为 want 的 box。
func findBox(f io.ReadSeeker, start, end int64, want string) (*boxHeader, error) {
	for off := start; off < end; {
		b, err := nextBox(f, off, end)
		if err != nil {
			return nil, err
		}
		if b.typ == want {
			return b, nil
		}
		off = b.end
	}
	return nil, errBoxNotFound
}

// parseMvhd 从 moov→mvhd 读出 timescale 和 duration。
//
// 偏移量的来历（v0：timescale 在 payload 第 12 字节、duration 在第 16）：
//
//	version(1) + flags(3)           = 4
//	creation_time(4)                = 8
//	modification_time(4)            = 12   ← timescale 从这里开始
//	timescale(4)                    = 16   ← duration 从这里开始
//	duration(4)
//
// v1 把两个时间戳和 duration 都加宽成 8 字节，于是整体后移 8。
func parseMvhd(f io.ReadSeeker, b *boxHeader) (timescale uint32, duration uint64, err error) {
	if b.payloadLen() < 20 {
		return 0, 0, errors.New("mvhd 太短")
	}
	if _, err = f.Seek(b.payloadOff, io.SeekStart); err != nil {
		return 0, 0, err
	}
	var buf [32]byte
	if _, err = io.ReadFull(f, buf[:]); err != nil {
		return 0, 0, err
	}

	if buf[0] == 1 {
		// v1：creation(8) + modification(8) → timescale 在 20，duration 在 24
		timescale = binary.BigEndian.Uint32(buf[20:24])
		duration = binary.BigEndian.Uint64(buf[24:32])
	} else {
		timescale = binary.BigEndian.Uint32(buf[12:16])
		duration = uint64(binary.BigEndian.Uint32(buf[16:20]))
	}
	return timescale, duration, nil
}

// parseTkhd 从 trak→tkhd 读出宽高。两个都是 **16.16 定点数**，取高 16 位就是整数像素。
//
// 偏移量的来历（v0：width 在 payload 第 76 字节）：
//
//	version(1)+flags(3)   = 4
//	creation(4)           = 8
//	modification(4)       = 12
//	track_ID(4)           = 16
//	reserved(4)           = 20
//	duration(4)           = 24
//	reserved(8)           = 32
//	layer(2)              = 34
//	alternate_group(2)    = 36
//	volume(2)             = 38
//	reserved(2)           = 40
//	matrix(36)            = 76   ← width 从这里开始
//	width(4)              = 80   ← height 从这里开始
//	height(4)             = 84
//
// v1 同样把时间戳和 duration 加宽，整体后移 12。
func parseTkhd(f io.ReadSeeker, b *boxHeader) (w, h int, err error) {
	var wOff int64
	var need int64
	if b.payloadLen() < 1 {
		return 0, 0, errors.New("tkhd 太短")
	}
	if _, err = f.Seek(b.payloadOff, io.SeekStart); err != nil {
		return 0, 0, err
	}
	var ver [1]byte
	if _, err = io.ReadFull(f, ver[:]); err != nil {
		return 0, 0, err
	}
	if ver[0] == 1 {
		wOff, need = 88, 96
	} else {
		wOff, need = 76, 84
	}
	if b.payloadLen() < need {
		return 0, 0, fmt.Errorf("tkhd 太短: %d < %d", b.payloadLen(), need)
	}

	if _, err = f.Seek(b.payloadOff, io.SeekStart); err != nil {
		return 0, 0, err
	}
	buf := make([]byte, need)
	if _, err = io.ReadFull(f, buf); err != nil {
		return 0, 0, err
	}

	// 高 16 位是整数部分，低 16 位是小数（实际几乎总是 0）
	w = int(binary.BigEndian.Uint32(buf[wOff:wOff+4]) >> 16)
	h = int(binary.BigEndian.Uint32(buf[wOff+4:wOff+8]) >> 16)
	return w, h, nil
}

// ProbeMP4 读一个本地 mp4 文件的时长/尺寸/码率。失败返回 error。
//
// 调用方（video_service.Publish）**不能因为这里报错就让发布失败** ——
// 探测是"决定要不要转码"的输入，不是发布的前置条件。
func ProbeMP4(path string) (*MP4Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()

	moov, err := findBox(f, 0, size, "moov")
	if err != nil {
		// 顶层没有 moov → 不是 mp4（或者被截断到连 moov 都没有）
		return nil, fmt.Errorf("没找到 moov: %w", err)
	}

	info := &MP4Info{SizeBytes: size}

	// ---------- 时长 ----------
	if mvhd, err := findBox(f, moov.payloadOff, moov.end, "mvhd"); err == nil {
		timescale, duration, err := parseMvhd(f, mvhd)
		if err == nil && timescale > 0 {
			info.DurationMS = int64(float64(duration) / float64(timescale) * 1000)
		}
	}
	// mvhd 缺失/解析失败**不返回错误**：宽高依然有用，而且码率算不出来时
	// 调用方还能靠"文件多大"做兜底判断。少一个字段比整个探测失败好。

	// ---------- 宽高：在**视频那条** trak 里 ----------
	//
	// moov 里通常有两条以上 trak（视频一条、音频一条）。音频 trak 的 tkhd
	// 宽高都是 0，所以判据是"大于 0"。多视频轨（极少见）取面积最大的那条。
	//
	// 没有去读 hdlr 的 handler_type == "vide"：那要多解析一层 box，
	// 而"面积最大的非零 tkhd"在这个用途上和它等价 —— 目标是知道
	// "画面多大"，不是"这条轨是什么编码"。
	for off := moov.payloadOff; off < moov.end; {
		trak, err := nextBox(f, off, moov.end)
		if err != nil {
			break
		}
		off = trak.end
		if trak.typ != "trak" {
			continue
		}
		tkhd, err := findBox(f, trak.payloadOff, trak.end, "tkhd")
		if err != nil {
			continue
		}
		w, h, err := parseTkhd(f, tkhd)
		if err != nil || w <= 0 || h <= 0 {
			continue
		}
		if w*h > info.Width*info.Height {
			info.Width, info.Height = w, h
		}
	}

	// ---------- 码率 ----------
	if info.DurationMS > 0 {
		// 用毫秒算，避免先转秒再除造成的精度损失（短视频尤其明显）。
		// 量纲：bytes×8 = bits，再除以毫秒 = bit/ms = **kbit/s**。
		// 所以这里不需要任何额外的 ×1000 或 ÷1000。
		info.BitrateKbps = int(size * 8 / info.DurationMS)
	}
	return info, nil
}

// probeResult 门禁探测的完整结果。
//
// 带上 status 而不是"info == nil 就代表失败"，是因为**失败有两种**，
// 而它们在排查时指向完全不同的方向：
//
//	status = failed, info = nil   文件不在 / 不是 mp4 / 截断
//	status = ok,     info = nil   不可能（探到 moov 就会有 info）
type probeResult struct {
	status string
	info   *MP4Info
}

// probeSource 按 PlayURL 反查磁盘路径并探测。
//
// **这个函数永远不会返回 error，也永远不会让发布失败。** 它只回答
// "这条素材该不该转码"，而回答不了的时候（文件不在、路径不对、格式不认识）
// 答案是"不转，直传"—— 发布本身必须成功。
//
// 三条失败路径，都归到 ProbeStatusFailed：
//
//  1. PlayURL 不是 /static/ 开头，或者试图跳出 UploadRoot（见 config.DiskPath）
//  2. 文件不存在。**这是正常情况**：上传和发布是解耦的（文件在
//     chunk_handler 的 os.Rename 落地，videos 行在 Publish 才建），
//     所以理论上文件应该已经在。但"理论上"三个字在这里值一次 os.Stat ——
//     手工清理过、迁移过、或者客户端伪造了一个 play_url，都会走到这。
//  3. 文件在，但不是能解析的 mp4。
//
// 三种都**只记日志**，不返回错误。这是刻意的：门禁是优化，不是校验。
func (vs *VideoService) probeSource(playURL string) probeResult {
	path, ok := vs.storage.DiskPath(playURL)
	if !ok {
		// 不记 Warn 级别：play_url 由客户端传，恶意/错误的输入是常态，
		// 每次都刷一行日志会淹掉真正的告警。Publish 那边已经会记一条总的。
		return probeResult{status: ProbeStatusFailed}
	}

	info, err := ProbeMP4(path)
	if err != nil {
		// 文件不存在是最常见的一种，单独把它和"解析失败"分开记 ——
		// 前者通常意味着上传/发布之间有别的 bug，后者说明素材本身有问题。
		if _, statErr := os.Stat(path); statErr != nil {
			log.Printf("[probeSource] 源文件不在: %s (%v)", path, statErr)
		} else {
			log.Printf("[probeSource] 解析失败: %s (%v)", path, err)
		}
		return probeResult{status: ProbeStatusFailed}
	}
	return probeResult{status: ProbeStatusOK, info: info}
}
