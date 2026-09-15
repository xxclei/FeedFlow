package transcode

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"myfeed/internal/video"
)

// 本文件的测试全部**不需要 ffmpeg**。
//
// 这不是巧合，是 plan.go / playlist.go 的结构刻意换来的：
//
//	Plan / OutputSize / ScaleFilter / BuildArgs   纯函数 → 断言字符串
//	MasterPlaylist / ParseMaster                  纯函数 → 断言文本
//	VerifyMaster / CleanupRuns / MasterRunIDs     只碰临时目录 → t.TempDir()
//	Run                                           **唯一**一个要真跑 ffmpeg 的
//
// 分隔线画在 Run 上：它是"执行"那一层，其余都是"准备"那一层。
// 一个需要 45 MB 素材、跑十几秒的性能测试，如果和"输出尺寸算得对不对"
// 挤在同一个文件里，后者就永远不会被人主动跑。
//
// ⚠ 有一件事本文件**测不到**：Args 拼错了但拼成了一个"ffmpeg 也接受、
// 只是结果不对"的命令（比如漏了 -b:v 而 -maxrate 还在）。
// 那种错只能靠真跑一次 + ffprobe 核对，见 scripts/bench-notes-transcode.md
// 的验收记录。**纯函数测试证明不了"参数是有效的"，只能证明"参数是我算出来的那串"。**

// ---------- 输出尺寸 ----------

func TestOutputSize(t *testing.T) {
	tier := func(name string) video.Tier {
		for _, x := range video.DefaultTiers {
			if x.Name == name {
				return x
			}
		}
		t.Fatalf("档位表里没有 %s", name)
		return video.Tier{}
	}

	tests := []struct {
		name         string
		tier         string
		srcW, srcH   int
		wantW, wantH int
		why          string
	}{
		{"横屏 1080p 源 → 1080p 档", "1080p", 1920, 1080, 1920, 1080, "输出等于源，不该缩放"},
		{"横屏 1080p 源 → 720p 档", "720p", 1920, 1080, 1280, 720, "锁高，宽按比例"},
		{"横屏 1080p 源 → 480p 档", "480p", 1920, 1080, 854, 480, "1920×480/1080=853.33 → FFALIGN 取 854"},
		{"横屏 720p 源 → 720p 档", "720p", 1280, 720, 1280, 720, "输出等于源"},
		{"横屏 720p 源 → 480p 档", "480p", 1280, 720, 854, 480, "同样落在 853.33 → 854 这个边界上"},
		{"竖屏 1080×1920 → 1080p 档", "1080p", 1080, 1920, 1080, 1920, "竖屏的 1080p 短边是宽"},
		{"竖屏 1080×1920 → 480p 档", "480p", 1080, 1920, 480, 854, "锁宽，高按比例：1920×480/1080=853.33→854"},
		{"带黑边的超宽 1920×800 → 720p", "720p", 1920, 800, 1728, 720, "锁高，长边 1920×720/800=1728"},
		{"奇数比例 1001×667 → 480p", "480p", 1001, 667, 720, 480, "1001×480/667=720.6→720（截断后取偶）"},
		{"正方形 1000×1000", "480p", 1000, 1000, 480, 480, "srcW>=srcH 走横屏分支，短边还是 480"},
		{"源尺寸未知", "1080p", 0, 0, 1920, 1080, "回落 16:9 名义值，只为不 panic"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, h := OutputSize(tier(tt.tier), tt.srcW, tt.srcH)
			if w != tt.wantW || h != tt.wantH {
				t.Errorf("OutputSize(%s, %dx%d) = %dx%d, want %dx%d（%s）",
					tt.tier, tt.srcW, tt.srcH, w, h, tt.wantW, tt.wantH, tt.why)
			}
		})
	}
}

// TestOutputSizeAlwaysEven 是上面那张表**推不出来的**一条性质。
//
// 表的每一行都是一个具体数字，而这里要的是"任何输入都不出奇数" ——
// 那是个全称命题，只能扫。而它值得扫，因为奇数宽高的后果是
// ffmpeg 悄悄改成奇数-1（画面对不上 RESOLUTION）或者直接失败。
func TestOutputSizeAlwaysEven(t *testing.T) {
	for _, tt := range video.DefaultTiers {
		for w := 100; w <= 4001; w += 137 { // 质数步长，避免和除数共振
			for h := 100; h <= 4001; h += 149 {
				gotW, gotH := OutputSize(tt, w, h)
				if gotW%2 != 0 || gotH%2 != 0 {
					t.Fatalf("OutputSize(%s, %dx%d) = %dx%d 出了奇数", tt.Name, w, h, gotW, gotH)
				}
				if gotW < 2 || gotH < 2 {
					t.Fatalf("OutputSize(%s, %dx%d) = %dx%d 太小", tt.Name, w, h, gotW, gotH)
				}
			}
		}
	}
}

// ---------- 缩放表达式 ----------

func TestScaleFilter(t *testing.T) {
	tests := []struct {
		name       string
		outW, outH int
		srcW, srcH int
		want       string
	}{
		{"横屏需要缩放", 1280, 720, 1920, 1080, "scale=-2:720,setsar=1"},
		{"竖屏需要缩放", 480, 854, 1080, 1920, "scale=480:-2,setsar=1"},
		{"尺寸相同则不缩放", 1920, 1080, 1920, 1080, "setsar=1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScaleFilter(Job{OutW: tt.outW, OutH: tt.outH}, tt.srcW, tt.srcH)
			if got != tt.want {
				t.Errorf("ScaleFilter = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestScaleFilterAlwaysSetsar 钉住一个**很容易在重构里丢掉**的东西。
//
// 尺寸相同那条分支（"不缩放"）是最容易被简化成 `return ""` 的 ——
// 而那样一来，SAR≠1 的源（手机录屏、DVD 转制）转出来会被播放器二次拉伸。
// 表现是"画面被压扁了"，而 master 里的 RESOLUTION 写的是存储尺寸、
// 看起来完全正常。
func TestScaleFilterAlwaysSetsar(t *testing.T) {
	for _, tt := range []struct{ outW, outH, srcW, srcH int }{
		{1920, 1080, 1920, 1080}, // 不缩放
		{1280, 720, 1920, 1080},  // 缩放
		{0, 0, 0, 0},             // 源尺寸未知
	} {
		got := ScaleFilter(Job{OutW: tt.outW, OutH: tt.outH}, tt.srcW, tt.srcH)
		if !strings.Contains(got, "setsar=1") {
			t.Errorf("ScaleFilter(%+v) = %q 里没有 setsar=1", tt, got)
		}
	}
}

// ---------- ffmpeg 参数 ----------

func TestBuildArgs(t *testing.T) {
	tier := video.Tier{Name: "720p", Height: 720, VideoKbps: 2500, AudioKbps: 128}
	args := buildArgs("src.mp4", tier, filepath.Join("out", "720p"), 1280, 720, 1920, 1080)
	got := strings.Join(args, " ")

	// 必须出现的（每一条都对应 buildArgs 注释里的一件事）
	must := []struct{ needle, why string }{
		{"-nostdin", "不加的话后台进程里的 ffmpeg 会卡在读 stdin"},
		{"-map 0:v:0", "不显式取第一路视频的话，内嵌封面图可能被当成主视频"},
		{"-map 0:a:0?", "问号是静音视频不报错的唯一原因"},
		{"-vf scale=-2:720,setsar=1", "缩放链"},
		{"-pix_fmt yuv420p", "兼容性底线"},
		{"-profile:v main", "最大兼容档"},
		{"-b:v 2500k", "ABR 目标码率 —— 过载降级的核算基础"},
		{"-maxrate 2500k", "不封顶的话复杂画面会冲上去，码率就不可预测了"},
		{"-bufsize 5000k", "2 倍目标"},
		{"-b:a 128k", "音轨码率，TotalKbps 的另一半"},
		{"-force_key_frames expr:gte(t,n_forced*6)", "和 SegmentSeconds 对齐，否则会出碎分片"},
		{"-hls_time 6", "分片时长"},
		{"-hls_playlist_type vod", "写 ENDLIST，播放器才知道总时长"},
		{"-hls_flags independent_segments", "切档不清缓冲的前提"},
		{filepath.Join("out", "720p", "seg_%05d.ts"), "分片命名"},
		{filepath.Join("out", "720p", "index.m3u8"), "分档 playlist 是最后一个参数（输出）"},
	}
	for _, m := range must {
		if !strings.Contains(got, m.needle) {
			t.Errorf("参数里缺 %q（%s）\n实际: %s", m.needle, m.why, got)
		}
	}

	// 必须**不**出现的：CRF 是 ABR 的反面，两个同时给会让 -b:v 失效
	if strings.Contains(got, "-crf") {
		t.Errorf("出现了 -crf，它会让 -b:v 失效（码率重新变得不可预测）: %s", got)
	}
}

// TestBuildArgsBandwidthMatchesTierTable 是"码率可预测"这条不变量
// 在纯函数层唯一能测的部分：**命令里写的码率和档位表是同一个数**。
//
// 它防的是有人为了调画质单独改了 buildArgs 里的字面量（比如把 2500 写死成 3000），
// 那样转码产出 3000 kbps、而降级按 2628 算负载 —— 每一档都差一点，
// 人一多就系统性偏移，且不报错。
func TestBuildArgsBandwidthMatchesTierTable(t *testing.T) {
	for _, tier := range video.DefaultTiers {
		args := buildArgs("src.mp4", tier, "out", 1920, 1080, 1920, 1080)
		joined := strings.Join(args, " ")
		wantV := "-b:v " + itoa(tier.VideoKbps) + "k"
		wantA := "-b:a " + itoa(tier.AudioKbps) + "k"
		if !strings.Contains(joined, wantV) {
			t.Errorf("%s 档缺 %q", tier.Name, wantV)
		}
		if !strings.Contains(joined, wantA) {
			t.Errorf("%s 档缺 %q", tier.Name, wantA)
		}
	}
}

func itoa(v int) string { return strconv.Itoa(v) }

// ---------- Plan ----------

func TestPlan(t *testing.T) {
	tiers := []video.Tier{{Name: "1080p", Height: 1080, VideoKbps: 4500, AudioKbps: 128}}
	jobs := Plan("src.mp4", 1920, 1080, filepath.Join("uploads", "hls", "7", "abc123"), tiers)

	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	j := jobs[0]
	if j.Dir != filepath.Join("uploads", "hls", "7", "abc123", "1080p") {
		t.Errorf("Dir = %q", j.Dir)
	}
	if j.OutW != 1920 || j.OutH != 1080 {
		t.Errorf("OutW/OutH = %dx%d, want 1920x1080", j.OutW, j.OutH)
	}
	if len(j.Args) == 0 {
		t.Error("Args 是空的")
	}
	// 最后两个参数必须落在这一档自己的目录里 —— 写错会让三档互相覆盖
	last := j.Args[len(j.Args)-1]
	if !strings.HasPrefix(last, j.Dir) {
		t.Errorf("输出路径 %q 不在本档目录 %q 下", last, j.Dir)
	}
}

// ---------- master playlist ----------

func TestMasterPlaylistRoundTrip(t *testing.T) {
	jobs := []Job{
		{Tier: video.DefaultTiers[0], OutW: 1920, OutH: 1080},
		{Tier: video.DefaultTiers[1], OutW: 1280, OutH: 720},
		{Tier: video.DefaultTiers[2], OutW: 854, OutH: 480},
	}
	text := MasterPlaylist("abc123", jobs)

	if !strings.HasPrefix(text, "#EXTM3U\n") {
		t.Error("master 必须首行是 #EXTM3U —— 少了它整个文件不是 playlist")
	}
	if !strings.Contains(text, "#EXT-X-VERSION:3") {
		t.Error("缺 EXT-X-VERSION:3（浮点 EXTINF 需要它）")
	}
	if !strings.Contains(text, "#EXT-X-INDEPENDENT-SEGMENTS") {
		t.Error("缺 EXT-X-INDEPENDENT-SEGMENTS（切档会伴随可见卡顿）")
	}
	// 手写一遍期望值。这正是"不写 CODECS"那条决策的落点：
	// 一旦有人加了 CODECS，这条断言会红，而那正是需要重新论证的时刻。
	want := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-INDEPENDENT-SEGMENTS\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=4628000,RESOLUTION=1920x1080\nabc123/1080p/index.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=2628000,RESOLUTION=1280x720\nabc123/720p/index.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1128000,RESOLUTION=854x480\nabc123/480p/index.m3u8\n"
	if text != want {
		t.Errorf("master 内容不符\n实际:\n%s\n期望:\n%s", text, want)
	}

	// 回读：解析出来的东西要和写进去的一致
	vs := ParseMaster(text)
	if len(vs) != 3 {
		t.Fatalf("解析出 %d 档, want 3", len(vs))
	}
	for i, v := range vs {
		if v.Bandwidth != jobs[i].Tier.TotalKbps()*1000 {
			t.Errorf("第 %d 档带宽 = %d, want %d（TotalKbps 含音轨）",
				i, v.Bandwidth, jobs[i].Tier.TotalKbps()*1000)
		}
		if v.Width != jobs[i].OutW || v.Height != jobs[i].OutH {
			t.Errorf("第 %d 档分辨率 = %dx%d, want %dx%d",
				i, v.Width, v.Height, jobs[i].OutW, jobs[i].OutH)
		}
		if v.TierName() != jobs[i].Tier.Name {
			t.Errorf("第 %d 档名 = %q, want %q", i, v.TierName(), jobs[i].Tier.Name)
		}
		if v.RunID() != "abc123" {
			t.Errorf("第 %d 档 runID = %q", i, v.RunID())
		}
	}
}

// TestMasterPlaylistBandwidthIncludesAudio 单独钉住"含音轨"这件事。
//
// 它看起来和上面那条重复，其实测的是**不同的失败**：上面测的是
// "BANDWIDTH == TotalKbps*1000"（自我一致），这条测的是
// "TotalKbps 真的把音轨算进去了"（对外正确）。
// 只写上面那条的话，把 TotalKbps 改成 return VideoKbps 两边会一起变，
// 断言照样全绿。
func TestMasterPlaylistBandwidthIncludesAudio(t *testing.T) {
	tier := video.Tier{Name: "480p", Height: 480, VideoKbps: 1000, AudioKbps: 128}
	vs := ParseMaster(MasterPlaylist("r", []Job{{Tier: tier, OutW: 854, OutH: 480}}))
	if len(vs) != 1 {
		t.Fatalf("解析出 %d 档", len(vs))
	}
	if vs[0].Bandwidth != 1128000 {
		t.Errorf("BANDWIDTH = %d, want 1128000（1000 video + 128 audio = 1128 kbps）", vs[0].Bandwidth)
	}
}

// TestParseMasterToleratesForeignFiles 解析器是**读**的那一侧，
// 它会读到别的东西写出来的 master（ffmpeg 生成的、手写的、将来别的版本的）。
// 所以它必须容忍缺属性、多属性、空行、注释。
func TestParseMasterToleratesForeignFiles(t *testing.T) {
	text := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-INDEPENDENT-SEGMENTS

#EXT-X-STREAM-INF:BANDWIDTH=5000000,CODECS="avc1.640028,mp4a.40.2",RESOLUTION=1920x1080,FRAME-RATE=30.000
1080p/index.m3u8

#EXT-X-STREAM-INF:RESOLUTION=1280x720
720p/index.m3u8

#EXT-X-STREAM-INF:BANDWIDTH=800000
480p/index.m3u8
`
	vs := ParseMaster(text)
	if len(vs) != 3 {
		t.Fatalf("解析出 %d 档, want 3（缺属性不该让整档消失）", len(vs))
	}
	if vs[0].Bandwidth != 5000000 || vs[0].Width != 1920 {
		t.Errorf("第 1 档 = %+v（多出来的 CODECS/FRAME-RATE 不该影响解析）", vs[0])
	}
	if vs[1].Bandwidth != -1 || vs[1].Width != 1280 {
		t.Errorf("第 2 档 = %+v（缺 BANDWIDTH 应记 -1 而不是 0）", vs[1])
	}
	if vs[2].Height != 0 || vs[2].Bandwidth != 800000 {
		t.Errorf("第 3 档 = %+v（缺 RESOLUTION 应记 0 而不是拒绝整档）", vs[2])
	}
	// 空行隔开的 URI 也要能找到
	if vs[0].URI != "1080p/index.m3u8" {
		t.Errorf("URI = %q", vs[0].URI)
	}
}

func TestParseMasterEmpty(t *testing.T) {
	for _, text := range []string{"", "#EXTM3U\n", "#EXT-X-STREAM-INF:BANDWIDTH=1\n"} {
		if vs := ParseMaster(text); len(vs) != 0 {
			t.Errorf("ParseMaster(%q) = %+v, want 空", text, vs)
		}
	}
}

// ---------- VerifyMaster：用真实临时目录 ----------

// writeProduct 造一棵最小的产物树，形状和 worker 真写出来的一样。
//
// segs = -1 表示"playlist 存在但一个分片都没有"（半截产物的形态）。
func writeProduct(t *testing.T, root, runID string, tiers []video.Tier, segs int) string {
	t.Helper()
	var jobs []Job
	for _, tt := range tiers {
		dir := filepath.Join(root, runID, tt.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if segs >= 0 {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-PLAYLIST-TYPE:VOD\n")
			for i := 0; i < segs; i++ {
				b.WriteString("#EXTINF:6.000000,\nseg_0000" + strconv.Itoa(i) + ".ts\n")
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte(b.String()), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		jobs = append(jobs, Job{Tier: tt, OutW: 1920, OutH: tt.Height})
	}
	master := filepath.Join(root, "master.m3u8")
	if err := WriteMaster(master, MasterPlaylist(runID, jobs)); err != nil {
		t.Fatal(err)
	}
	return master
}

func TestVerifyMaster(t *testing.T) {
	t.Run("完整的三档产物", func(t *testing.T) {
		root := t.TempDir()
		master := writeProduct(t, root, "run1", video.DefaultTiers, 3)
		n, err := VerifyMaster(master)
		if err != nil {
			t.Fatalf("应当通过: %v", err)
		}
		if n != 3 {
			t.Errorf("可播档数 = %d, want 3", n)
		}
	})

	t.Run("半截产物：playlist 空", func(t *testing.T) {
		root := t.TempDir()
		master := writeProduct(t, root, "run1", video.DefaultTiers, -1)
		if _, err := VerifyMaster(master); err == nil {
			t.Fatal("playlist 是空的却通过了 —— 这正是重投时必须挡住的那种产物")
		}
	})

	t.Run("playlist 有但 0 个分片", func(t *testing.T) {
		root := t.TempDir()
		master := writeProduct(t, root, "run1", video.DefaultTiers, 0)
		if _, err := VerifyMaster(master); err == nil {
			t.Fatal("0 分片却通过了")
		}
	})

	t.Run("少了一档的目录", func(t *testing.T) {
		root := t.TempDir()
		master := writeProduct(t, root, "run1", video.DefaultTiers, 3)
		if err := os.RemoveAll(filepath.Join(root, "run1", "720p")); err != nil {
			t.Fatal(err)
		}
		n, err := VerifyMaster(master)
		if err == nil {
			t.Fatal("少一档却通过了")
		}
		// 部分可用也要报出来：阶段 E 会拿它判断"现在还剩几档能发"
		if n != 2 {
			t.Errorf("可播档数 = %d, want 2", n)
		}
	})

	t.Run("master 不存在", func(t *testing.T) {
		if _, err := VerifyMaster(filepath.Join(t.TempDir(), "nope.m3u8")); err == nil {
			t.Fatal("文件不存在却通过了")
		}
	})

	t.Run("master 是空文件", func(t *testing.T) {
		root := t.TempDir()
		p := filepath.Join(root, "master.m3u8")
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyMaster(p); err == nil {
			t.Fatal("空 master 却通过了")
		}
	})
}

// ---------- 清理 ----------

func TestCleanupRuns(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"old1", "old2", "keep"} {
		if err := os.MkdirAll(filepath.Join(root, name, "1080p"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// master.m3u8 是 root 下的一个**文件**，清理绝不能碰它
	master := filepath.Join(root, "master.m3u8")
	if err := os.WriteFile(master, []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed := CleanupRuns(root, []string{"keep"})
	if removed != 2 {
		t.Errorf("删了 %d 个, want 2", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "keep", "1080p")); err != nil {
		t.Errorf("被引用的目录被删掉了: %v", err)
	}
	if _, err := os.Stat(master); err != nil {
		t.Errorf("master.m3u8 被删掉了 —— 清理必须只动目录: %v", err)
	}

	// 幂等 + 不存在的 root 不 panic
	if n := CleanupRuns(root, []string{"keep"}); n != 0 {
		t.Errorf("第二次调用删了 %d 个, want 0", n)
	}
	if n := CleanupRuns(filepath.Join(root, "nope"), nil); n != 0 {
		t.Errorf("root 不存在时删了 %d 个, want 0", n)
	}
}

// TestCleanupKeepsWhatMasterReferences 是"keep 从 master 推导"这条不变量的
// **端到端**验证。它防的是调用方改成自己记 runID：
// 那条路上任何一次忘记更新，删掉的就是正在被别人播的那棵树。
func TestCleanupKeepsWhatMasterReferences(t *testing.T) {
	root := t.TempDir()

	// 先造一次"旧的"运行，再造一次"新的"，master 指向新的
	writeProduct(t, root, "oldrun", video.DefaultTiers, 2)
	if err := os.MkdirAll(filepath.Join(root, "oldrun"), 0o755); err != nil {
		t.Fatal(err)
	}
	master := writeProduct(t, root, "newrun", video.DefaultTiers, 2)
	if err := os.MkdirAll(filepath.Join(root, "oldrun"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 再造一个"从没被任何 master 引用过"的垃圾目录
	if err := os.MkdirAll(filepath.Join(root, "junk", "480p"), 0o755); err != nil {
		t.Fatal(err)
	}

	keep := MasterRunIDs(master)
	if len(keep) != 1 || keep[0] != "newrun" {
		t.Fatalf("MasterRunIDs = %v, want [newrun]", keep)
	}
	if n := CleanupRuns(root, keep); n != 2 {
		t.Errorf("删了 %d 个, want 2（oldrun + junk）", n)
	}
	for _, want := range []string{"newrun"} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Errorf("%s 被删了: %v", want, err)
		}
	}
	for _, gone := range []string{"oldrun", "junk"} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Errorf("%s 还在（err=%v）", gone, err)
		}
	}
	// 清完之后产物仍然可用 —— 这是整个设计的目的
	if _, err := VerifyMaster(master); err != nil {
		t.Errorf("清理之后 master 不可用了: %v", err)
	}
}

// ---------- 提交的原子性 ----------

func TestWriteMasterOverwrites(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "master.m3u8")
	if err := WriteMaster(p, "第一次\n"); err != nil {
		t.Fatal(err)
	}
	if err := WriteMaster(p, "第二次\n"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "第二次\n" {
		t.Errorf("内容 = %q, want 第二次（覆盖失败的话旧 master 会一直生效，"+
			"表现为「转完码还是老的档位」）", got)
	}
	// tmp 不能留在磁盘上：它出现在 /static 下就是一个能被下到的半截 master
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp 残留了（err=%v）", err)
	}
}

func TestDirSize(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "x.ts"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "m3u8"), []byte("123"), 0o644); err != nil {
		t.Fatal(err)
	}
	bytes, files := DirSize(root)
	if bytes != 8 || files != 2 {
		t.Errorf("DirSize = %d 字节 / %d 文件, want 8 / 2", bytes, files)
	}
	if bytes, files := DirSize(filepath.Join(root, "nope")); bytes != 0 || files != 0 {
		t.Errorf("不存在的目录 = %d/%d, want 0/0", bytes, files)
	}
}

func TestNewRunID(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := NewRunID()
		if len(id) != 16 {
			t.Fatalf("runID = %q（长度 %d）, want 16 位 hex", id, len(id))
		}
		if seen[id] {
			t.Fatalf("runID 撞了: %s", id)
		}
		seen[id] = true
	}
}
