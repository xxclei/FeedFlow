// Package qoe 播放质量埋点（QoE = Quality of Experience）。
//
// ---------- 这个模块为什么存在 ----------
//
// 2026-09-15 测完视频分发路径（见 scripts/bench-notes-video-delivery.md），结论是
// **瓶颈在 12 Mbps 上行，服务端差了 960 倍**。但那次测量只能算出"应该"卡不卡 ——
// 全部结论都建立在"缓冲字节数 ÷ 每人带宽"的算术上，**没有一个数字来自真实播放器**。
//
// 那份记录自己在 §八「明确没有测到的」里承认了这件事：
//   - 卡顿率：要做真卡顿率得要真浏览器 + 远端客户端，本次只测了"首帧时间"这个代理指标
//   - Chrome preload="auto" 的实际预加载字节数：没量，需要真浏览器 + 抓包
//
// 这个模块就是来补那一块的。它是后续所有播放侧优化的**仪表盘**：
// ABR 切档、过载降级、缓存头，每一个都得先有基线数字，否则"优化有效"只是感觉。
//
// ---------- 为什么一次播放只存一行 ----------
//
// 埋点最自然的形态是"一事件一行"（play / waiting / playing / error ...），
// 但那是**采集**的形态，不是**存储**的形态。一次播放会产生几十到上千条事件，
// 而所有要回答的问题都是聚合口径：
//
//	首帧 P95 是多少？     → 需要 startup_ms 的分布
//	卡顿率多高？           → 需要"有卡顿的会话占比"
//	平均码率多少？         → 需要各会话的均值
//
// 这些没有一个是"第 137 条事件是什么"。所以**在客户端聚合完再上报**，
// 一次播放落一行。代价是丢失了事件级的时序（无法回答"第 30 秒发生了什么"），
// 换来的是存储和查询都小两个数量级 —— 这个取舍在"看趋势"这个用途下是划算的。
// （真要事件级时序，那是另一条路：采样 + 专门的时序库，不是加一张表。）
//
// ---------- 三个核心指标，排序是有理由的 ----------
//
// 工业界对 QoE 的排序是 **卡顿 > 码率稳定 > 首帧**，不是直觉上的"首帧最重要"。
// 依据是哥伦比亚大学对 40 万+ YouTube 会话的研究：
//
//	一次卡顿对流失率的影响 ≈ 三次码率切换
//	首帧每多 1 秒，流失率 +5.8%
//	**中途把码率调高**（画质变好！）竟然也让流失率涨了 4 倍 —— 因为切换本身是干扰
//
// 所以这张表的字段也是按这个优先级铺的：stall_* 最前，其次是码率相关，
// startup_ms 排第三。这不是字段顺序的洁癖，**它决定了做看板时先看哪个数**。
//
// 完整的 QoE 模型（ITU-T P.1203，输出 MOS 1~5）是：
//
//	QoE = 感知画质(VMAF) − 卡顿惩罚 − 首帧惩罚 + 稳定性
//
// 本项目**不做**这个加权公式 —— 那需要主观评分数据集来标定权重，
// 没有的话算出来的 MOS 只是个好看的假数。存原始指标、看分布，更有用也更诚实。
package qoe

import "time"

// QoEEvent 一次播放会话的聚合结果。表名 → `qoe_events`。
//
// ---------- session_id 上的唯一索引是必需的，不是可选的 ----------
//
// 客户端有**三条**刷出路径（组件卸载 / 页面隐藏 / 页面卸载），而且它们可能
// 在同一次播放里全部触发。如果 session_id 没有唯一索引，同一次播放会落三行 ——
// 首帧 P95 会被同一批数据重复计入，卡顿率分母直接翻三倍。
//
// 加了唯一索引还不够：第二次上报会撞 1062。所以 repo 那边是 **upsert**
// （见 repo.go 的 Report）。两者是配套的：
//
//	唯一索引  保证不会重复计数
//	upsert    保证重复上报不会报错，且**后写的覆盖先写的**
//
// "后写覆盖"正好是对的语义：三条刷出路径里，越晚的那次携带的数据越完整
// （计数器只增不减），所以最后落库的就是最全的那份快照。
type QoEEvent struct {
	ID uint `gorm:"primaryKey" json:"id"`

	// SessionID 由**客户端**生成（crypto.randomUUID）。为什么不让服务端生成：
	// 三个刷出时机里有两个（pagehide、visibilitychange）发生在页面正在消失的时候，
	// 那时再去要一个 id 意味着一次额外的往返 —— 而 beacon 是"发了就不管"的，
	// 拿不到响应。客户端生成 = 上报是无状态的，不需要任何前置请求。
	SessionID string `gorm:"type:varchar(64);not null;uniqueIndex:idx_qoe_session" json:"session_id"`

	// VideoID 没有外键（全库都不用外键，见 message/service.go 里那段说明）。
	// 后果是一样的：video 被删了，这些行会变成孤儿。**这是可接受的** ——
	// 埋点数据本来就是历史事实，"这条视频当时播得怎么样"在视频删除后依然成立。
	VideoID uint `gorm:"index:idx_qoe_video_time,priority:1" json:"video_id"`

	// AccountID = 0 表示**游客**。不是"未知"，是真的没登录。
	//
	// 这里和项目里其他表不一样：别处 account_id 为 0 是脏数据（说明漏了鉴权），
	// 这里 0 是**合法且常见**的取值 —— 播放页 /video/:id 刻意是公开路由，
	// 游客能看能报。所以 report 接口挂的是 SoftJWTAuth 而不是 JWTAuth：
	// 软鉴权下没 token 就放行、handler 里降级成 0，跟 /feed 同一套路。
	//
	// 代价是没有 `default:0` 之外的保护 —— 一个 bug 让登录用户也报 0，
	// 表现是"游客占比异常高"，不会报错。看板要留意这个数。
	AccountID uint `gorm:"index;not null;default:0" json:"account_id"`

	// ---------- 卡顿（第一优先）----------

	// StallCount 一次播放里"开始等缓冲"的次数。
	// 判定见前端 useQoE.ts：waiting 事件计数，不是 stalled（两者语义不同）。
	StallCount int `gorm:"not null;default:0" json:"stall_count"`

	// StallTotalMS 所有卡顿的**累计毫秒**。
	//
	// 为什么 count 和 total 都要存：两者回答不同的问题。
	// 10 次 100ms 的卡顿（几乎无感）和 1 次 1000ms 的卡顿（很难受）
	// count 是 10 vs 1，total 是 1000 vs 1000 —— **任一单独看都会得出错误结论**。
	// 工业界的常用口径 ratio = stall_total_ms / played_ms 用的是后者。
	StallTotalMS int64 `gorm:"not null;default:0" json:"stall_total_ms"`

	// PlayedMS 真正在播的毫秒数（不含暂停）。这是卡顿率的分母。
	// 前端只在 !paused 时累加 —— 否则用户暂停去泡咖啡的时间会被算成播放时长，
	// 卡顿率被系统性低估。
	PlayedMS int64 `gorm:"not null;default:0" json:"played_ms"`

	// ---------- 码率（第二优先）----------

	// AvgBitrateKbps HLS 播放时实际拉到的平均码率（按播放时长加权）。
	//
	// **这个字段同时是"是不是走 HLS"的判别式**：直传 mp4 那条路没有档位概念，
	// 恒为 0。所以 `avg_bitrate_kbps > 0` ≡ 这条记录来自 HLS 播放。
	// 单独加一个 is_hls 布尔列是冗余的 —— 而且那个列会出现
	// "is_hls=true 但 avg_bitrate_kbps=0"（HLS 只有单档、没切过）这种自相矛盾的行。
	AvgBitrateKbps int `gorm:"not null;default:0" json:"avg_bitrate_kbps"`

	// TierSwitches 档位切换次数。稳定性的度量 —— 注意**不是越多越好也不是越少越好**：
	// 0 次可能是"很稳定"，也可能是"永远卡在最低档"（带宽一直不够，ABR 放弃了）。
	// 必须和 AvgBitrateKbps 一起看才能区分这两种情况。
	TierSwitches int `gorm:"not null;default:0" json:"tier_switches"`

	// ---------- 首帧（第三优先）----------

	// StartupMS = loadstart → 首次 playing。
	//
	// 为什么不用 play 事件：play 只表示"播放被请求了"，此刻一帧都没解出来。
	// 用 play 会得到一个偏小的、不反映用户观感的数 —— 前端 VideoPlayer.vue
	// 里收浮层用的也是 playing 而不是 play，同一条理由，两处保持一致。
	StartupMS int64 `gorm:"not null;default:0" json:"startup_ms"`

	// ---------- 上下文 ----------

	// DroppedFrames 丢帧数。客户端渲染能力不足的信号，与网络无关 ——
	// 带宽充裕但 CPU 弱（低端手机）时它会涨，此时降码率**没有用**。
	// 存它是为了能把"网络导致的差"和"解码导致的差"分开。
	DroppedFrames int64 `gorm:"not null;default:0" json:"dropped_frames"`

	// ErrorCode 媒体错误码（MediaError.code：1 ABORTED / 2 NETWORK / 3 DECODE / 4 SRC_NOT_SUPPORTED）。
	// 空字符串 = 没出错。用字符串而不是 int 是为了让空值语义明确 ——
	// int 的 0 既可能是"没错误"也可能是"忘了填"，字符串空值一眼能分辨。
	ErrorCode string `gorm:"type:varchar(32)" json:"error_code,omitempty"`

	// EffectiveType / DownlinkMbps 来自 navigator.connection（Network Information API）。
	//
	// ⚠ 两者都**只在 Chromium 系可用**，Firefox/Safari 上拿不到（会留空/0）。
	// 而且是**粗粒度**的：effective_type 只有 slow-2g/2g/3g/4g 四档，
	// downlink 是个被浏览器刻意模糊化的估计值（防指纹追踪）。
	//
	// 存它们的理由不是"精确测量带宽"，而是**分组**：把"4g 用户的卡顿率"
	// 和"3g 用户的卡顿率"分开看，比看总体均值有用得多。
	// 拿它当带宽真值用是错的 —— 真值只有服务端侧能测。
	EffectiveType string  `gorm:"type:varchar(16)" json:"effective_type,omitempty"`
	DownlinkMbps  float64 `gorm:"not null;default:0" json:"downlink_mbps"`

	// UserAgent 用于排查**特定浏览器/版本的播放问题**（"只有 Safari 卡顿"这类）。
	//
	// json:"-" 且外面不暴露：它是排查用的内部字段，不是给看板看的。
	// 注意 255 字节会截断真实 UA（现代 Chrome UA 约 120 字符，够用；
	// 但 UA 里可能带设备型号等信息，属于**弱标识**，前端要不要发由前端决定）。
	UserAgent string `gorm:"type:varchar(255)" json:"-"`

	// 和 video_id 组成复合索引，支撑"某条视频最近的表现"这个最高频查询。
	// 单列 video_id 索引不够：那个查询永远带时间范围，两列才走得动。
	//
	// ⚠ 它是**首次写入**的时刻，重复上报不会刷新它（upsert 的 DoUpdates
	// 列表里没有它，实测确认过）。这是刻意的：created_at 表示"这次播放
	// 什么时候开始的"，正好是 stats 时间窗要的口径 —— 要的是稳定，不是最新。
	// 但由此带来一个坑：handler 响应里回显的 created_at 是**本次请求**的时刻，
	// 重复上报时会比库里的新。详见 handler.Report 顶上那段。
	CreatedAt time.Time `gorm:"autoCreateTime;index:idx_qoe_video_time,priority:2" json:"created_at"`
}

// TableName 显式钉死表名。**这个方法不能删。**
//
// ---------- 不写它会怎样 ----------
//
// 不写的话 GORM 按自己的命名策略把 `QoEEvent` 转成表名，结果是
// **`qo_e_events`** —— 不是 `qoe_events`。原因是 GORM 的 toDBName 遇到
// 连续大写会在中间插下划线，而 `QoE` 这个缩写**不在 GORM 内置的
// commonInitialisms 表**里（那张表只有 ID / URL / HTTP / API 这类），
// 所以它被拆成了 `Qo_` + `E_` + `Event`。
//
// 实测确认：AutoMigrate 建出来的表就是 `qo_e_events`。
//
// ---------- 为什么这是"必须显式"而不是"无所谓" ----------
//
// 因为 repo.go 里 **StartupPercentiles 那条分位数查询是裸 SQL**
// （CTE + 窗口函数用 GORM 的链式 API 写不出来），里面硬编码了表名。
// 于是模型解析出的名字和裸 SQL 里的名字成了**两份**：
//
//	GORM 走模型  → qo_e_events   （Create / Model(&QoEEvent{}) 全走这条）
//	裸 SQL       → qoe_events    （只有 StartupPercentiles 走这条）
//
// 两条不一致的后果**不是启动报错**：AutoMigrate 会老老实实建出 qo_e_events，
// 写入也全部成功 —— 只有 /qoe/stats 的百分位查询会炸 "table doesn't exist"。
// 也就是说，坏掉的是看板上最显眼的那个数（首帧 P50/P95），
// 而写入侧一切正常，排查时很容易往错的方向找。
//
// 钉死成 `qoe_events` 而不是反过来把裸 SQL 改成 `qo_e_events`，理由是
// **`qo_e_events` 是个没人能猜到的名字** —— 文档、注释、看板、以及将来
// 任何一条手写 SQL 都会自然而然地写成 `qoe_events`。让它当权威，
// 裸 SQL 就永远不会和它分叉。
func (QoEEvent) TableName() string {
	return "qoe_events"
}

// ReportQoERequest 客户端上报的请求体。字段与 QoEEvent 一一对应，
// 但**没有 ID / CreatedAt / UserAgent** —— 前两个由服务端定，
// UA 从请求头取（客户端自报的 UA 没有意义，那是可以随便写的字符串）。
//
// ⚠ 手工 curl 时注意（见 memory: gin-ignores-unknown-json-fields）：
// gin **忽略未知字段**，写成 sessionId 或 sessionID 不会报错，
// 只会被静默忽略，然后 session_id 保持空串 → 400。
// **看起来像校验太严，实际是字段名写错了。**
type ReportQoERequest struct {
	SessionID string `json:"session_id"`
	VideoID   uint   `json:"video_id"`

	StallCount   int   `json:"stall_count"`
	StallTotalMS int64 `json:"stall_total_ms"`
	PlayedMS     int64 `json:"played_ms"`

	AvgBitrateKbps int `json:"avg_bitrate_kbps"`
	TierSwitches   int `json:"tier_switches"`

	StartupMS     int64  `json:"startup_ms"`
	DroppedFrames int64  `json:"dropped_frames"`
	ErrorCode     string `json:"error_code"`

	EffectiveType string  `json:"effective_type"`
	DownlinkMbps  float64 `json:"downlink_mbps"`
}

// StatsRequest 聚合查询的请求体。VideoID = 0 表示**全部视频**。
//
// 为什么 0 表示全部而不是"报错"：这是唯一一个"不筛条件"的自然表达，
// 而且聚合看板的第一屏就是"总体怎么样"。想只查某条就传 id。
//
// ---------- Days 的三个取值区间（别把 0 和负数搞混）----------
//
//	没传（零值 0）→ handler 替换成默认 7 天
//	正数          → 就查这么多天
//	**负数        → 不限时间（全时段）**
//
// 为什么不让 0 直接表示"不限时间"：那样"没传"和"要全时段"就没法区分了，
// 而它们的语义完全相反（前者是"看最近"，后者是"看全部"）。
// 详细取舍见 handler.go 里 Stats 的注释。
type StatsRequest struct {
	VideoID uint `json:"video_id"`
	Days    int  `json:"days"`
}

// StartupPercentiles 首帧时间的分位数。**三个都给，不给单个均值。**
//
// 为什么均值在这里是骗人的：首帧分布是**长尾**的 ——
// 大多数会话 1~2 秒，少数走 3G 的会到 20 秒。均值会被长尾拖到一个
// "没有任何一个用户真实经历过"的位置（比如 P50=1.3s 均值=4.1s）。
// 工业界的验收标准也全是分位数口径（"首帧 P95 < 2s"），不是均值口径。
//
// P99 也要给：它就是那些"最惨的用户"，而 QoE 的整个意义就在于别让他们存在。
type StartupPercentiles struct {
	N   int64 `json:"n"`
	P50 int64 `json:"p50_ms"`
	P95 int64 `json:"p95_ms"`
	P99 int64 `json:"p99_ms"`
}

// TierBucket 码率分桶。桶的边界是**固定的**（不是按数据分位），
// 因为跨时间对比时桶必须稳定 —— 今天"0.5~1 Mbps"这个桶和下周的必须是同一个含义。
//
// 边界选 500/1500/3000 的理由：它们大致对应 360p / 480p~720p / 1080p 的典型码率，
// 所以桶名对人有意义（"大部分人在 720p 档"），而不只是"第三桶最大"。
type TierBucket struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// StatsResponse 聚合结果。
//
// 刻意**不包含**一个总分/总评级。理由在 entity.go 顶上说过：
// 没有标定权重就不该合成一个 MOS，那只会得到一个"看起来很专业"的假数。
// 这里只给原始分布，让读的人自己看。
type StatsResponse struct {
	TotalSessions int64 `json:"total_sessions"`

	// Guests 是 total 里 account_id=0 的那部分。单独给出是因为它异常升高
	// 就说明鉴权接线出问题了（见 QoEEvent.AccountID 的说明）。
	Guests int64 `json:"guests"`

	// StalledSessions 有**至少一次**卡顿的会话数。卡顿率 = 它 / total。
	//
	// 分母用 total（全部会话）而不是"播放超过 N 秒的会话"——
	// 后者更严谨（3 秒就退出的会话没机会卡顿），但那需要定 N，
	// 而定 N 得有数据支撑。先用最朴素的口径，把 N 这件事写在这里备查。
	StalledSessions int64 `json:"stalled_sessions"`

	// StallRatio 累计卡顿时长 / 累计播放时长。这是上面那个"卡顿率"的**另一种口径**。
	//
	// 两个都要给，因为它们会不一致，而**不一致本身是信息**：
	//	会话数口径高、时长口径低 → 很多次很短的卡顿（烦人但不难受）
	//	会话数口径低、时长口径高 → 少数几次很长的卡顿（难受，且这些人大概率流失了）
	StallRatio float64 `json:"stall_ratio"`

	Startup StartupPercentiles `json:"startup"`

	// AvgBitrateKbps 只在 HLS 会话上算（avg_bitrate_kbps > 0 的那些）。
	// 把直传会话的 0 混进来会把均值拉到一个无意义的数上。
	AvgBitrateKbps    int64        `json:"avg_bitrate_kbps"`
	BitrateSessions   int64        `json:"bitrate_sessions"`
	Tiers             []TierBucket `json:"tiers"`
	TotalDropped      int64        `json:"total_dropped_frames"`
	TotalTierSwitches int64        `json:"total_tier_switches"`
}
