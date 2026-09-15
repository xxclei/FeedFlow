package transcode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 本文件负责 master playlist 的**读写**。为什么是手写而不是让 ffmpeg 生成：
//
//	ffmpeg 能用 `-var_stream_map` + `-master_pl_name` 一次生成 master，
//	但那样 master 就成了一个**静态文件** —— 而阶段 E 的全部内容就是
//	"同一个 URL 下按负载返回不同的档位列表"。静态文件做不到这件事。
//
// 所以 master 由我们自己写，ffmpeg 只负责生成每档的 index.m3u8 和分片。
// 顺带的好处：RESOLUTION / BANDWIDTH 这两个属性的值是我们算的，
// 而不是从 ffmpeg 的推导里读出来的 —— 它们和过载降级用的是同一份档位表，
// 于是"播放器按 X kbps 选档"和"服务端按 X kbps 算负载"不可能对不上。

// MasterPlaylist 生成 master.m3u8 的内容。
//
// runID 是**当前这一次运行**的目录名 —— master 里的分档 URI 是相对 master
// 自身位置的（`{runID}/1080p/index.m3u8`），所以 master 一被重写，
// 整棵产物树就一起换了。这就是"提交点"的实现方式：一次写文件，全换或全不换。
//
// ---------- 为什么没有 CODECS 属性 ----------
//
// HLS 规范里 `#EXT-X-STREAM-INF` 的 CODECS 是**可选**的，而写错它的后果
// 比不写严重得多：hls.js 在有 CODECS 时会**按它过滤档位**，
// 声明了 `avc1.640028`（High 4.0）而设备/浏览器不支持时就静默丢掉这一档 ——
// 表现是"转码成功但只有两档能播"，日志里什么都没有。
//
// 而我们**写不出正确的 CODECS**：它的 level 部分取决于分辨率×帧率
// （1080p30 是 4.0，1080p60 要 4.2），而帧率我们没有规范化
// （源多少帧就多少帧）。写死一个 level 就是在赌用户传的都是 30fps。
//
// 不写 CODECS 时 hls.js 会等第一个分片真的到了、从 SPS 里读真实的编解码参数 ——
// 慢一点点，但永远是对的。**在一个"静默失效"比"慢一点"贵得多的项目里，
// 这个取舍不用犹豫。**
//
// 同理没写 FRAME-RATE：源帧率未归一，写它等于编一个数。
func MasterPlaylist(runID string, jobs []Job) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	// VERSION 3 = 支持浮点 EXTINF（分片时长不是整数时必须有它）。
	// 没有它播放器会按版本 1 解析，浮点数时长会被当成非法值。
	b.WriteString("#EXT-X-VERSION:3\n")
	// INDEPENDENT-SEGMENTS：和 ffmpeg 那边的 `-hls_flags independent_segments`
	// 是同一件事的两个声明处（一个在分档 playlist 里，一个在 master 里）。
	// 缺了它 hls.js 切档时倾向于先清缓冲再切，切档会伴随一次可见的卡顿。
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	for _, j := range jobs {
		// BANDWIDTH 用 TotalKbps（含音轨）**而不是 VideoKbps**：
		//
		//   · 对播放器：hls.js 用它做初始档位估算，含音轨才是真实占用
		//   · 对服务端：阶段 E 的负载核算是 Σ TotalKbps，两边同一个数
		//
		// 漏掉音轨的话每档低估 128 kbps —— 单个人看不出，人一多就系统性偏移，
		// 而这正是"降级阈值要准"最怕的那种错。
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d\n",
			j.Tier.TotalKbps()*1000, j.OutW, j.OutH)
		fmt.Fprintf(&b, "%s/%s/index.m3u8\n", runID, j.Tier.Name)
	}
	return b.String()
}

// Variant master 里的一档。**它是"解析 master"的产物，不是"生成 master"的输入**
// （生成的输入是 Job）—— 这个不对称是刻意的：读的那一侧要能容忍
// 别人（或别的版本的本程序）写出来的 master，写的这一侧不用。
type Variant struct {
	// Bandwidth 位/秒（HLS 规范的单位，**不是 kbps**）
	Bandwidth int
	// Width / Height RESOLUTION 属性，读不出来时是 0
	Width  int
	Height int
	// URI 相对 master 的路径，形如 `9f3a1c02/1080p/index.m3u8`
	URI string
}

// TierName 从 URI 里取档位名（`9f3a1c02/1080p/index.m3u8` → `1080p`）。
//
// 用目录名当档位名是**布局约定**（见 config.HLSRelDir 那段），
// 而档位名和档位表的 Name 是同一个字符串 —— 所以这个函数
// 是"从产物反查档位"的唯一入口。取不出来时返回空串，调用方按"不认识"处理。
func (v Variant) TierName() string {
	segs := strings.Split(v.URI, "/")
	if len(segs) < 2 {
		return ""
	}
	return segs[len(segs)-2]
}

// RunID 从 URI 里取这次运行的目录名（第一段）。
func (v Variant) RunID() string {
	if i := strings.IndexByte(v.URI, '/'); i > 0 {
		return v.URI[:i]
	}
	return ""
}

// ParseMaster 解析一份 master.m3u8。
//
// 容错取向：**能读懂多少算多少，读不懂的行跳过**，不返回 error。
// 理由是它的调用者（VerifyMaster / 阶段 E 的动态 master）要做的事
// 都是"在能读懂的档位里挑"，而不是"这份 master 是否完全合规"——
// 为一个属性缺失就整份拒绝，会把"能播"降级成"不播"。
// 真正的坏情况（一个档位都读不出来）由调用方判 len==0。
func ParseMaster(text string) []Variant {
	var out []Variant
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		v := Variant{Bandwidth: -1}
		for _, attr := range strings.Split(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"), ",") {
			k, val, ok := strings.Cut(strings.TrimSpace(attr), "=")
			if !ok {
				continue
			}
			switch strings.TrimSpace(k) {
			case "BANDWIDTH":
				v.Bandwidth, _ = strconv.Atoi(strings.Trim(val, `"`))
			case "RESOLUTION":
				w, h, ok := strings.Cut(strings.Trim(val, `"`), "x")
				if !ok {
					continue
				}
				v.Width, _ = strconv.Atoi(w)
				v.Height, _ = strconv.Atoi(h)
			}
		}
		// 属性行后面第一条非空、非注释行就是 URI。
		for i++; i < len(lines); i++ {
			uri := strings.TrimSpace(lines[i])
			if uri == "" || strings.HasPrefix(uri, "#") {
				continue
			}
			v.URI = uri
			break
		}
		if v.URI != "" {
			out = append(out, v)
		}
	}
	return out
}

// VerifyMaster 检查一份 master 引用的每档都真的可播。
//
// 返回**可播档数**，不是"有没有问题"的 bool —— 因为调用方要把它写进日志
// （"三档就绪"），也要在阶段 E 里拿它判断"降级之后还剩几档"。
//
// 检查的是"每档的 playlist 至少有一个分片"，而不是"分片文件都在"：
// 后者要把每个 .ts 都 stat 一遍，而 hls muxer 是先写 playlist 还是先写
// 分片不是我们能控制的 —— 检查过头会把一次成功的转码判成失败。
// **它能证明的是"这份 master 指向的东西不是空的"，这恰好是重投/续跑
// 判据需要的强度**：半截产物的 playlist 是空的（ffmpeg 直到第一个分片
// 写完才写 playlist），所以它挡得住我们真正担心的那一件事。
func VerifyMaster(masterPath string) (int, error) {
	data, err := os.ReadFile(masterPath)
	if err != nil {
		return 0, err
	}
	variants := ParseMaster(string(data))
	if len(variants) == 0 {
		return 0, errors.New("master 里没有任何档位")
	}

	dir := filepath.Dir(masterPath)
	ok := 0
	var problems []string
	for _, v := range variants {
		p := filepath.Join(dir, filepath.FromSlash(v.URI))
		n, err := countSegments(p)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", v.URI, err))
			continue
		}
		if n == 0 {
			problems = append(problems, v.URI+": 分片列表是空的")
			continue
		}
		ok++
	}
	if len(problems) > 0 {
		return ok, fmt.Errorf("%d/%d 档不可用: %s",
			len(problems), len(variants), strings.Join(problems, "; "))
	}
	return ok, nil
}

// countSegments 数一份分档 playlist 里有几个分片。
func countSegments(playlistPath string) (int, error) {
	data, err := os.ReadFile(playlistPath)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#EXTINF:") {
			n++
		}
	}
	return n, nil
}

// CleanupRuns 删掉 `hls/<id>/` 下**没有被当前 master 引用**的运行目录。
//
// ---------- 为什么是在成功之后删，而不是在开始之前清 ----------
//
// 计划里原本的写法是"重跑前先清空输出目录"。加了一层 runID 之后这件事
// 反过来了（见 config/paths.go 那段）：新产物写在自己的新目录里，
// master 直到全部写完才被重写，所以**任何时刻都不存在"master 指向半截产物"**，
// 开始之前不需要清。而"删旧的"必须在**新的已经生效之后**做 ——
// 顺序反了的话，删完旧的、新的一写就失败，视频就彻底没有可播的产物了。
//
// keep 从当前 master 里推导（调用方传 MasterRunIDs 的结果），
// 所以它天然只保留"真被引用的"那一份，不依赖调用方记得传对 runID。
//
// 返回删掉的目录数。删不掉**不返回错误**：清理失败不该让一次成功的转码
// 变成失败（那会触发重投 → 白白重跑一遍分钟级任务）。调用方记日志即可。
func CleanupRuns(hlsRoot string, keep []string) int {
	entries, err := os.ReadDir(hlsRoot)
	if err != nil {
		return 0
	}
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[k] = true
	}

	removed := 0
	for _, e := range entries {
		if !e.IsDir() || keepSet[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(hlsRoot, e.Name())); err != nil {
			continue
		}
		removed++
	}
	return removed
}

// MasterRunIDs 从一份 master 里取出它引用的全部运行目录名。
//
// 单独一个函数（而不是让调用方自己 From ParseMaster 里 map 一下），
// 是因为"keep 应该等于 master 引用的东西"这条不变量值得有个名字 ——
// CleanupRuns 的安全性完全建立在它上面。
func MasterRunIDs(masterPath string) []string {
	data, err := os.ReadFile(masterPath)
	if err != nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, v := range ParseMaster(string(data)) {
		if id := v.RunID(); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// WriteMaster 原子地写 master.m3u8：先写 `.tmp`，再 rename 覆盖。
//
// **rename 是这次转码的提交点。** 直接 os.WriteFile 的话，
// 写到一半被杀会留下一份**截断的 master** —— 而它指向的目录是完整的，
// 于是播放器读到一个语法上合法、内容少了一半的清单，
// 表现是"少了一档"或者"一个档位都播不出来"，且看不出是半截文件。
// rename 在同一个文件系统内是原子的，所以读者看到的永远是
// "旧的完整版"或"新的完整版"。
//
// ⚠ Windows 上 os.Rename 覆盖已存在的文件**是可行的**
// （走的是 MoveFileEx + MOVEFILE_REPLACE_EXISTING），
// 但**目标被占用时会失败**（有进程正打开着 master.m3u8 读）。
// 这个窗口极短（静态文件服务读完就关），遇到了就让转码失败重投一次 ——
// 比在这里加一个重试循环简单，而且这种情况本来就该重来一次。
func WriteMaster(masterPath, content string) error {
	tmp := masterPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, masterPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// DirSize 统计一个目录的字节数、文件数，用于报告磁盘占用。
//
// 统计失败不返回错误（部分结果也值得看）：它的唯一用途是日志。
func DirSize(root string) (bytes int64, files int) {
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // 见函数头：统计失败按"这部分不算"处理
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		bytes += info.Size()
		files++
		return nil
	})
	return bytes, files
}
