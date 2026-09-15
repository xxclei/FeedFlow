package qoe

import (
	"context"
	"strings"
)

// 上限常量。**每一个都对应一种"客户端坏了"的具体形态**，不是为了好看。
//
// 埋点是全项目唯一一个"数据由不受信任的客户端算好了再交上来"的接口 ——
// 别处服务端都能自己验证（下没下单、点没点赞），这里**验证不了**：
// 我们没法复现用户的网络去核对"这次卡了 3 秒"。所以只能做**范围校验**：
// 拦不住谎报，但拦得住溢出和垃圾值。
//
// 换句话说：**这些常量是"防脏"，不是"防伪"。** 想防伪得在客户端侧
// 做完整性签名，而那本身也拦不住有决心的攻击者 —— 对一个学习项目不值得。
// 真正重要的是：一个负号或一个 int64 溢出能让整张表的聚合结果全错，
// 那是**可以**防住的，成本也就是几个 if。
const (
	// maxSessionIDLen 对应 varchar(64)。超长会在 MySQL 侧报错（严格模式）
	// 或静默截断（非严格模式），两种都比直接拒绝差 ——
	// 静默截断尤其坏：两个 session 的 id 前缀相同就会撞唯一索引，
	// 表现是"某次播放的上报丢失"，而且只在特定 id 组合下出现。
	maxSessionIDLen = 64

	// maxPlayedMS = 24 小时。一条视频不可能播 24 小时 ——
	// 真有这种数字，只可能是客户端计时器算错了（比如把 `Date.now()`
	// 的单位当成秒、或者组件被复用时没重置累加器）。
	// 不拦的话它会成为一个极大的分母，把卡顿率压到接近 0，
	// **让整个看板显得"一切正常"** —— 这是最坏的一类脏数据：它让监控失效。
	maxPlayedMS = 24 * 60 * 60 * 1000

	// maxStallTotalMS 同样按 24 小时封顶。合理值应该远小于 played_ms
	// （卡顿时长不可能超过播放时长），但这里**不强制这个约束** ——
	// 因为"卡顿计时和播放计时用了不同时钟"导致的轻微越界是真实存在的，
	// 卡在边界上会把正常数据也拒掉。留给看板去看异常，不在写入口拦。
	maxStallTotalMS = 24 * 60 * 60 * 1000

	// maxStartupMS = 10 分钟。超过这个数用户早走了，不会真的在等 ——
	// 这个值几乎必然是"页面被挂起了"（手机切后台时浏览器会冻结计时器，
	// 恢复后一个巨大的时间差被记进 startup）。
	maxStartupMS = 10 * 60 * 1000

	// maxBitrateKbps = 100 Mbps。远超任何真实档位（手机原片也就 15 Mbps），
	// 但留足余量，避免把将来可能加的 4K 档误拦。
	maxBitrateKbps = 100_000

	// maxCount 给 stall_count / tier_switches / dropped_frames 用。
	// 100000 是个"绝不可能但也不会误伤"的数 —— 它的作用只是挡住
	// int 溢出和明显荒谬的值，不需要精确。
	maxCount = 100_000

	// maxDownlinkMbps = 10 Gbps。navigator.connection.downlink 的规范上限。
	maxDownlinkMbps = 10_000.0
)

// QoEService 埋点业务。**这一层做的事只有一件：把客户端交上来的数字削进合法范围。**
//
// 为什么清洗放在 service 而不是 handler：清洗是**数据规则**，不是 HTTP 形状问题。
// handler 该管的是"字段名对不对、必填有没有"，service 管的是"这个数字合不合理"。
// 两者混在一起的话，将来加一个 gRPC 入口就要把清洗逻辑抄一遍。
//
// 对比 message/service.go：那边的 service 很薄是因为**业务规则只有一条**
// （收件人必须存在）。这边的 service 也很薄，但薄的原因不同 ——
// 这里根本没有"业务"，只有"防脏"。**同样薄，理由不一样。**
type QoEService struct {
	repo *QoERepository
}

func NewQoEService(repo *QoERepository) *QoEService {
	return &QoEService{repo: repo}
}

// Report 校验 + 清洗 + 落库。
//
// ---------- 关于"不校验 video_id 是否存在" ----------
//
// 这**偏离了本项目的既有纪律**，所以要说清楚。
//
// 项目里已经三次在 service 里手验引用完整性（social 的两端 FindByID、
// message 的收件人、comment/like 的 video 存在性），理由是"全库不用外键，
// 引用完整性就从数据库的活变成了 service 的活"。这里不跟，理由有三条：
//
//  1. **后果不对称。** message 不校验时，给不存在的 to_id 发消息会得到一条
//     "永远没人能读到"的行 —— 那是真正的数据垃圾。而 qoe_events 的
//     video_id 指向不存在的视频时，这一行**依然完全可读、可聚合**：
//     它就是一条"某次播放发生过"的历史记录。孤儿不等于垃圾。
//
//  2. **有一条合法路径会撞上这个检查。** 长视频播到一半时作者删了它，
//     客户端在页面卸载时才上报 —— 这时视频确实已经不在了。
//     校验会把这条**完全真实**的上报拒掉，造成数据丢失。
//     而它丢失得**毫无声响**（客户端正在卸载，收不到也处理不了错误码）。
//
//  3. **成本在错误的层。** 加校验就是给"每次播放"这个写路径加一次
//     SELECT —— 而它换来的只是"拦住一个查不到的 video_id"，
//     而那个 id 对应的统计永远不会被查询（没人会去查一条不存在的视频的 QoE）。
//
// 真要说风险，是"某个 bug 让 video_id 恒为 0"这类系统性问题 ——
// 但那在聚合结果里一眼可见（所有数据都挤在 video_id=0 上），
// **看板能发现它，比在写入口逐个拦更划算。**
func (s *QoEService) Report(ctx context.Context, req *ReportQoERequest, accountID uint, userAgent string) (*QoEEvent, error) {
	e := &QoEEvent{
		SessionID: strings.TrimSpace(req.SessionID),
		VideoID:   req.VideoID,
		// accountID = 0 是合法的（游客），见 entity.go 里那一列的说明。
		// 这里**不做**"没登录就拒绝"的判断 —— 那会让游客的播放完全不可见，
		// 而游客恰恰是最需要被观测的那群人（他们最容易因为卡顿直接流失，永不回来）。
		AccountID: accountID,

		StallCount:   clampInt(req.StallCount, maxCount),
		StallTotalMS: clampInt64(req.StallTotalMS, maxStallTotalMS),
		PlayedMS:     clampInt64(req.PlayedMS, maxPlayedMS),

		AvgBitrateKbps: clampInt(req.AvgBitrateKbps, maxBitrateKbps),
		TierSwitches:   clampInt(req.TierSwitches, maxCount),

		StartupMS:     clampInt64(req.StartupMS, maxStartupMS),
		DroppedFrames: clampInt64(req.DroppedFrames, maxCount),

		ErrorCode: truncate(strings.TrimSpace(req.ErrorCode), 32),

		EffectiveType: truncate(strings.TrimSpace(req.EffectiveType), 16),
		DownlinkMbps:  clampFloat(req.DownlinkMbps, maxDownlinkMbps),

		UserAgent: truncate(userAgent, 255),
	}

	if err := s.repo.Report(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

// Stats 聚合。days <= 0 表示不限时间，video_id = 0 表示全部视频。
//
// 四次查询串行发。**没有并发**——看着像可以起四个 goroutine，但：
// 它们打的是同一个连接池（maxOpenConns=100，够），而看板是低频调用，
// 串行省的这点延迟换不来任何东西，反而多出四个错误要处理。
// **低频路径上的并发是不必要的复杂度。**
func (s *QoEService) Stats(ctx context.Context, videoID uint, days int) (*StatsResponse, error) {
	totals, err := s.repo.Totals(ctx, videoID, days)
	if err != nil {
		return nil, err
	}

	startup, err := s.repo.StartupPercentiles(ctx, videoID, days)
	if err != nil {
		return nil, err
	}

	bitrate, err := s.repo.Bitrate(ctx, videoID, days)
	if err != nil {
		return nil, err
	}

	tiers, err := s.repo.Tiers(ctx, videoID, days)
	if err != nil {
		return nil, err
	}

	resp := &StatsResponse{
		TotalSessions:     totals.Total,
		Guests:            totals.Guests,
		StalledSessions:   totals.Stalled,
		Startup:           StartupPercentiles{N: startup.N, P50: startup.P50, P95: startup.P95, P99: startup.P99},
		AvgBitrateKbps:    bitrate.Avg,
		BitrateSessions:   bitrate.N,
		Tiers:             tiers,
		TotalDropped:      totals.Dropped,
		TotalTierSwitches: totals.Switches,
	}

	// 卡顿率 = 累计卡顿时长 / 累计播放时长。
	//
	// ⚠ 分母为 0 时必须短路，否则是 NaN（Go 里 0.0/0.0 得到 NaN，不是 panic）——
	// NaN 序列化成 JSON 会让 `json.Marshal` **直接报错**
	// （"unsupported value: NaN"），于是整个 stats 接口 500。
	// 空表时这一定会发生，所以这不是理论边界，是**第一次调用就会踩**的路径。
	if totals.PlayedMS > 0 {
		resp.StallRatio = float64(totals.StallMS) / float64(totals.PlayedMS)
	}

	return resp, nil
}

// ---------- 清洗工具 ----------
//
// 三个都是"削"而不是"拒"：非法值变成边界值，请求照常成功。
//
// 为什么削而不拒（和上面 session_id 的处理相反）：session_id 非法时
// 整条记录**没法存**（它是主键），只能拒。而这些数字非法时，
// 记录本身仍然是有效的一次播放 —— 拒掉等于为了一个字段丢掉整条数据。
// **能修的就修，修不了的才拒。**

func clampInt(v, max int) int {
	if v < 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

func clampInt64(v, max int64) int64 {
	if v < 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

func clampFloat(v, max float64) float64 {
	// NaN 要先单独挡：`NaN < 0` 和 `NaN > max` **都是 false**，
	// 所以下面两个 if 都拦不住它，它会一路走到数据库。
	// （MySQL 的 DOUBLE 存得下 NaN，然后 AVG 出来还是 NaN，
	//   再然后 json.Marshal 报错 —— 一个 NaN 能毒死整张表的聚合。）
	// JSON 里本来打不出 NaN，但 Go 的 float64 字段可以从别处进来，
	// 这个判断是为那个未来准备的。
	if v != v {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

// truncate 按**字节**截断（不是 rune）。
//
// 这里按字节是对的，因为这些字段在 MySQL 侧就是 varchar(N) —— 那是 N 个字符，
// 但对 ASCII 内容（UA、错误码、effective_type）字节数 == 字符数。
// 万一有人往 effective_type 里塞中文，按字节截断可能切出半个 UTF-8 字符，
// 存进 utf8mb4 列时会报错。**这是可接受的**：这些字段本来就只该是 ASCII 枚举值，
// 塞中文进来说明客户端有 bug，让它显式报错比静默存个乱码好。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
