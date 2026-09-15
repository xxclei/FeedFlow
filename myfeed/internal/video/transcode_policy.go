package video

import "fmt"

// 上传质量门禁的**判定策略**。和 IO 完全分开，所以它是纯函数、可以直接单测。
//
// ---------- 它在整个闭环里的位置 ----------
//
//	上传门禁探测（本文件）→ 超阈值入队 → 转码多档 → 播放端 ABR → 过载降级
//
// 门禁是**分流器，不是拒绝器**。它从不拒绝上传 —— 低于阈值就直接用原文件，
// 高码率才入队转码。所以它最坏情况下的后果是"这条没转"，而不是"用户发不出视频"。
//
// ---------- 为什么必须有它 ----------
//
// 在此之前，项目的码率 **100% 由上传者决定**：没有转码，传什么就播什么。
// bench-notes-video-delivery.md 实测的"能撑 33 人"，靠的是上传者碰巧传了
// 0.36 Mbps 的素材 —— 是运气，不是架构。有人传一条手机原片（1080p / 12 Mbps），
// 同一条上行能撑的人数立刻掉到"不到 1 人"。
//
// 这个文件就是把那个决定权收回服务端的第一步。

// ---------- 档位表 ----------

// Tier 一档转码输出。
//
// ⚠ **这张表有三个读者，改它要同时想三件事**：
//
//  1. transcode worker  —— 按它生成 ffmpeg 参数（分辨率 + 目标码率）
//  2. 过载降级（阶段 E）—— 按它算"当前负载"（Σ 每个观众正在拉的档位码率）
//  3. 本文件的判定策略   —— 按它算"源素材该不该转"
//
// 三处的口径必须是同一份，否则会出现"降级按 4500 算、转码按 3000 生成"这种
// 各算各的，表现为过载阈值系统性偏移，而且不报错。
type Tier struct {
	// Name 同时是输出子目录名（`1080p/index.m3u8`）和前端看到的档位标签。
	// 用分辨率当名字而不是编号：日志和排查里 "1080p" 一眼就懂，
	// "tier0" 得回去查表。
	Name string
	// Height 这一档的**短边**像素数。
	//
	// 为什么是短边而不是 height：竖屏视频（1080×1920）的"1080p"指的是
	// 短边 1080，它的 height 是 1920。用 height 当档位会让竖屏视频
	// 全都被算成"比 1080p 还大"。scale 的具体参数由 worker 按
	// 源素材的方向决定（横屏 scale=-2:1080，竖屏 scale=1080:-2）。
	Height int
	// VideoKbps 视频轨目标码率。**不含音轨** —— 负载核算用的是
	// VideoKbps + AudioKbps，别只看这一个。
	VideoKbps int
	// AudioKbps 音轨目标码率。三档统一 128 kbps：
	// 音频再往上（192/320）对"能不能流畅播"没有影响，
	// 而它占的带宽是实打实的（在 480p 那一档占了 11%）。
	AudioKbps int
}

// TotalKbps 这一档的实际总码率。**过载降级必须用这个而不是 VideoKbps** ——
// 用漏音轨的话，每档都低估 128 kbps，多人时误差会累积。
func (t Tier) TotalKbps() int { return t.VideoKbps + t.AudioKbps }

// DefaultTiers 三档输出，从高到低。
//
// 具体数字是 H.264 的常见经验值，不是算出来的：
//
//	1080p / 4500 kbps —— 1080p30 的"看起来不糊"的常用下限
//	 720p / 2500 kbps —— 720p30 约 2~3 Mbps 是主流平台的口径
//	 480p / 1000 kbps —— 手机小屏/弱网兜底档，1 Mbps 已经能看
//
// 和 12 Mbps 上行预算对照一下，这套档位的现实含义是：
//
//	全 1080p → 约 2 人    全 720p → 约 3 人    全 480p → 约 9 人
//
// —— 上行就是硬约束，转码改变不了它。转码能做的是**让 9 个人看 480p，
// 而不是 2 个人看 1080p 其余 7 个人卡死**。这就是过载降级要买的东西。
var DefaultTiers = []Tier{
	{Name: "1080p", Height: 1080, VideoKbps: 4500, AudioKbps: 128},
	{Name: "720p", Height: 720, VideoKbps: 2500, AudioKbps: 128},
	{Name: "480p", Height: 480, VideoKbps: 1000, AudioKbps: 128},
}

// ---------- 状态字面量 ----------
//
// 两列状态机一共七个取值。**写成常量而不是散落的字符串字面量**，
// 因为它们的读者分布在三个进程里（API 的 Publish、worker 的转码、
// 将来的清理任务），拼错一个字母的表现是"状态永远不匹配"而不报错。
//
// 注意这两组值都是**字符串列**（varchar(16)），不是枚举 ——
// 加一个状态不需要 ALTER TABLE。代价是没有数据库层的约束，
// 所以字面量只在这一个文件里定义。

// probe_status 的取值。
const (
	// ProbeStatusOK 探到了 moov，字段可用（时长可能仍为 0，见 SrcDurationMS）
	ProbeStatusOK = "ok"
	// ProbeStatusFailed 文件不在 / 不是 mp4 / 截断。表现为 decideTranscode 直传。
	ProbeStatusFailed = "failed"
)

// transcode_status 的取值。
const (
	// TranscodeUnknown 空串 = **这一行从未经过门禁**。
	//
	// 单独给它一个名字，而不是让空串匿名地躺在数据库里，因为它是一个
	// **真实且会长期存在**的状态：`transcode_status` 这一列是跟门禁一起
	// 加上去的，ALTER TABLE 给的默认值就是空串，**存量行不会被回填**。
	//
	// 实测：加上门禁之后，`select transcode_status, count(*) group by 1`
	// 出来是「空串 127 条 / skipped 1 条」—— 那 127 条就是本项目改造之前
	// 发布的视频。它们**事实上**都在直传（见下），但"事实直传"和
	// "门禁判定直传"是两回事，把它记成 skipped 就等于伪造了一次判定。
	//
	// 为什么它不影响功能：播放端只有 `== "ready"` 才走 HLS，空串自然走
	// 直传那条老路；过载降级只统计 HLS 会话，也不会碰它们。
	// 所以它的存在是**可观测性**问题（多一个取值要理解），不是 bug。
	// —— 给名字就是为了让读代码的人不用再猜一次。
	TranscodeUnknown = ""

	// TranscodeSkipped 门禁判定直传原文件（低码率，或探测失败）。
	//
	// 存量素材的**现状**是 TranscodeUnknown；若哪天真的重跑一遍门禁，
	// 它们的归宿会是这个值 —— 实测 135 个源文件全部如此
	// （128 个 1920×1080 但码率只有 0.16~1.16 Mbps，5 个 640×360，
	// 2 个 854×480，**没有一条超过任何档位目标 ×1.25**）。
	TranscodeSkipped = "skipped"
	// TranscodePending 已入队（outbox 行已写），等 worker 领。
	// ⚠ 一条视频长期停在这里 = worker 没在跑 / 且 MQ 是通的，
	// 这正是 outbox 模式想要的可观测性：卡住看得见。
	TranscodePending = "pending"
	// TranscodeRunning worker 已认领，正在转。
	//
	// 孤儿状态的风险：worker 被 kill -9 时它会永远停在 running。
	// 这是**刻意接受**的 —— 消息没有 Ack 就会被 MQ 重投，
	// 重跑时第一件事就是把状态改回 running（幂等），所以它自己会修复。
	// 不去做"超时后自动回退到 pending"的清理，是因为那需要另一个定时任务，
	// 而它要解决的问题（worker 中途死掉）已经被 MQ 的重投解决了。
	TranscodeRunning = "running"
	// TranscodeReady HLS 三档就绪，hls_url 可用。**这是唯一一个前端会走 HLS 的状态。**
	TranscodeReady = "ready"
	// TranscodeFailed 转码失败 → 仍然直传原文件，视频照常能播。
	//
	// 它和 "skipped" 的区别只在运维视角："skipped" 是计划内，
	// "failed" 是要去看的。对用户两者没有区别。
	TranscodeFailed = "failed"
)

// ---------- 判定阈值 ----------

const (
	// transcodeBitrateMargin 源码率超过目标码率这个倍数才转。
	//
	// 为什么不是 1.0（"超了就转"）：转码本身**是有损的**。
	// 一条 1080p / 5000 kbps 的素材，重新编成 4500 kbps 只会让画质更差、
	// 体积只小 10% —— 纯亏。留 25% 的余量，让"转码"只在真的能省下
	// 有意义带宽时才发生。
	//
	// 这个数也是**转码成本的分界线**：转码是分钟级 CPU 任务，
	// 门禁太松会让 worker 被一堆"转了也没用"的任务淹掉。
	transcodeBitrateMargin = 1.25

	// maxServeShortSide 我们最多服务到 1080p（短边）。
	//
	// 超过这个的一律转（不管码率多少）—— 因为 4K 素材即使码率"合理"，
	// 也远超这条 12 Mbps 的上行能承受的量，而且我们根本没有 4K 档位。
	maxServeShortSide = 1080
)

// Decision 门禁的判定结果。
type Decision struct {
	// Transcode true = 该入队转码；false = 直传原文件
	Transcode bool
	// Reason **给人和日志看的**，不参与任何逻辑。
	//
	// 单独带一个理由字符串，是因为运营/排查时第一个问题永远是
	// "这条为什么没转" —— 没有理由的话，只能对着阈值和数字自己推。
	// 它会进日志，也会在 needTranscode 为 false 时进 transcode_status 的上下文。
	Reason string
}

// shortSide 返回画面短边。竖屏视频按短边归档（见 Tier.Height 的说明）。
func shortSide(w, h int) int {
	if w < h {
		return w
	}
	return h
}

// targetForShortSide 返回"和这个源尺寸对应的档位"。
//
// 规则是**取短边不超过源短边的最大那档** —— 也就是"我们打算用它播的那个画质"。
// 源比最低档还小（短边 < 480）时返回 nil，表示**没有任何一档是合适的**：
// 把 360p 拉到 480p 只会更大更糊。
//
// 这里不返回 error 也不 panic，因为"没有合适的档位"是完全正常的情况
// （存量素材里就有），调用方按 nil 处理。
func targetForShortSide(ss int) *Tier {
	var best *Tier
	for i := range DefaultTiers {
		t := &DefaultTiers[i]
		if t.Height > ss {
			continue // 这一档比源还大，会变成放大
		}
		// DefaultTiers 从高到低，所以第一个满足的就是最大的那档
		if best == nil || t.Height > best.Height {
			best = t
		}
	}
	return best
}

// decideTranscode 门禁的核心判定。**纯函数** —— 没有 IO、没有时间、没有随机，
// 同样的输入永远同样的输出，所以它可以被穷举测试。
//
// 输入是探测结果（info 可以为 nil 表示探测失败）和探测是否成功。
//
// ---------- 判定顺序是有讲究的 ----------
//
// 先看"探测成没成"，再看"分辨率超没超"，最后才看码率。顺序不是随意的：
// 每一条都**覆盖**后面那条。比如一条 4K 素材，即使它的码率低得"合理"，
// 也应该转 —— 所以分辨率那条必须排在码率之前。
func decideTranscode(info *MP4Info, probeOK bool) Decision {
	// ---------- 1. 探测失败 → 直传 ----------
	//
	// 这是**保守**的选择，理由有两层：
	//
	//  a. 探测失败意味着文件有异常（截断/损坏/根本不是 mp4）。
	//     注意**不是**"布局不常见"—— 非 faststart（moov 在文件尾）的文件
	//     我们照样能解析，因为顶层 box 链是 seek 过去的。所以失败是真的坏。
	//     对坏文件去跑分钟级的 ffmpeg，换来的多半是也是失败，白烧 CPU。
	//
	//  b. 我们的降级方向一贯是**让视频能播**，不是"让策略生效"。
	//     直传最坏是慢，转码失败最坏是……也是直传。但中间白烧的 CPU
	//     会拖慢**别人**的转码（worker 是并发的，但机器 CPU 是有限的）。
	//
	// 代价：一条真正有问题但本可挽救的大文件会被放过去，以原码率服务。
	// 这个代价是可接受的 —— 它不影响任何人的播放，只是没优化到。
	if !probeOK || info == nil {
		return Decision{Transcode: false, Reason: "probe failed"}
	}

	ss := shortSide(info.Width, info.Height)

	// ---------- 2. 超出能服务的最大分辨率 → 转 ----------
	if ss > maxServeShortSide {
		return Decision{
			Transcode: true,
			Reason:    fmt.Sprintf("short side %d > %d", ss, maxServeShortSide),
		}
	}

	// ---------- 3. 比最低档还小 → 直传 ----------
	target := targetForShortSide(ss)
	if target == nil {
		return Decision{
			Transcode: false,
			Reason:    fmt.Sprintf("shorter than lowest tier (short side %d)", ss),
		}
	}

	// ---------- 4. 码率没超目标的可接受范围 → 直传 ----------
	//
	// info.BitrateKbps <= 0 表示时长算不出来（mvhd 缺失或 timescale 为 0），
	// 这时**无法判定码率**。选择直传而不是转：分辨率已经知道了，
	// 而"码率不明"更可能来自畸形文件 —— 和上面第 1 条同一个保守取向。
	if info.BitrateKbps <= 0 {
		return Decision{Transcode: false, Reason: "bitrate unknown (no mvhd)"}
	}

	limit := float64(target.VideoKbps) * transcodeBitrateMargin
	if float64(info.BitrateKbps) <= limit {
		return Decision{
			Transcode: false,
			Reason:    fmt.Sprintf("%d kbps <= %s target %d×%.2f", info.BitrateKbps, target.Name, target.VideoKbps, transcodeBitrateMargin),
		}
	}
	return Decision{
		Transcode: true,
		Reason:    fmt.Sprintf("%d kbps > %s target %d×%.2f", info.BitrateKbps, target.Name, target.VideoKbps, transcodeBitrateMargin),
	}
}

// UsableTiers 返回**源素材够得着的**档位（短边 >= 该档短边），从高到低。
//
// 给转码 worker 用：一条 720p 的源不应该产出 1080p 的档 ——
// 那是放大，只会更大更糊。worker 按这个列表逐个生成。
//
// 返回空切片表示"这一档都做不了"，这时调用方**不该入队转码**
// （decideTranscode 已经在第 3 条挡掉了这种情况，这里是双保险 ——
// worker 也可能被别的路径调用）。
//
// **导出**是因为它的调用方在另一个包（internal/worker 的转码 worker）。
// 这里没有顺手把它下沉到 transcode 包，因为"哪些档够得着"依赖
// DefaultTiers 的**语义**（Height 是短边），而那张表在这边 ——
// 挪过去的话，档位表的两个读者（判定/转码）会各自持有一半的知识。
func UsableTiers(w, h int) []Tier {
	ss := shortSide(w, h)
	out := make([]Tier, 0, len(DefaultTiers))
	for _, t := range DefaultTiers {
		if t.Height <= ss {
			out = append(out, t)
		}
	}
	return out
}
