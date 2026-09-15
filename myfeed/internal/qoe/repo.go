package qoe

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// QoERepository qoe_events 表读写：一个写入方法 + 一组聚合查询。
//
// 没有 UPDATE、没有 DELETE、没有事务、没有缓存。写侧只有一个 upsert，
// 读侧全是只读聚合 —— 这是全项目并发最简单的一张表（只增不改，
// 而且"改"还是幂等覆盖）。
//
// 读侧**刻意没有查单条的接口**。埋点数据的唯一用途是聚合，
// 提供一个 "按 id 查" 只会诱使调用方去写"看某一次播放的细节"这种查询 ——
// 而那张表根本没有细节可看（一次播放只有一行聚合结果）。
type QoERepository struct {
	db *gorm.DB
}

func NewQoERepository(db *gorm.DB) *QoERepository {
	return &QoERepository{db: db}
}

// Report 落库一次播放会话。**是 upsert，不是 insert。**
//
// ---------- 为什么必须是 upsert ----------
//
// 客户端有三条刷出路径（组件卸载 / 页面隐藏 / 页面卸载），它们可能全部触发。
// 纯 insert 的话第二次会撞唯一索引 1062，然后：
//   - 要么报错给客户端（客户端正在卸载页面，这个错误没人看得见，纯噪音）
//   - 要么吞掉（那"后写的覆盖先写的"这个正确语义就没了）
//
// upsert 两件事一起解决：不报错，且**最后一次上报获胜**。
// 而"最后获胜"恰好是对的 —— 越晚的刷出携带的计数器越大（卡顿只增不减），
// 所以最后落库的就是最完整的那份快照。
//
// ---------- 更新列里没有 session_id / video_id ----------
//
// 这两个是**键**，不是值。把 session_id 放进 DoUpdates 等于让它自己覆盖自己
// （无害但没意义）；把 video_id 放进去则是有害的 —— 那等于允许客户端
// 用同一个 session_id 把一条记录"搬家"到另一条视频上。不做。
//
// 反过来说：**同一次播放的 video_id 就不该变**，真变了说明客户端有 bug。
// 这里选择"忽略新值、保留旧值"，而不是报错 —— 报错会让整条上报失败，
// 连累那些本来正确的字段。
func (r *QoERepository) Report(ctx context.Context, e *QoEEvent) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "session_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"stall_count",
			"stall_total_ms",
			"played_ms",
			"avg_bitrate_kbps",
			"tier_switches",
			"startup_ms",
			"dropped_frames",
			"error_code",
			"effective_type",
			"downlink_mbps",
			"user_agent",
			// account_id 也更新：同一个 session_id 先以游客身份报、
			// 再带着 token 报（或反过来）在理论上可能发生。让它跟随最后一次。
			"account_id",
		}),
	}).Create(e).Error
}

// ---------- 聚合 ----------

// windowClause 生成时间窗条件。days <= 0 表示不限时间。
//
// 为什么默认要带时间窗：播放侧的任何改动（缓存头、ABR、降级）都会改变 QoE，
// 于是**跨改动的全时段均值必然是一个混合了两个世界的假数**。
// 看板默认只看最近 7 天，就是为了让数字始终对应"现在这份代码"。
func (r *QoERepository) windowClause(q *gorm.DB, days int) *gorm.DB {
	if days <= 0 {
		return q
	}
	return q.Where("created_at >= ?", time.Now().AddDate(0, 0, -days))
}

// totalsRow 是下面那条一次性聚合的扫描目标。
type totalsRow struct {
	Total    int64
	Guests   int64
	Stalled  int64
	PlayedMS int64
	StallMS  int64
	Dropped  int64
	Switches int64
}

// Totals 一次查询拿回所有"求和/计数"类的指标。
//
// 合成一条 SELECT 而不是发六条，是因为它们**扫的是同一批行** ——
// 分成六条意味着同一批行被读六遍。这在埋点表上尤其明显：它是全项目
// 唯一一张会无限增长的表（每次播放一行，用户越多增长越快）。
//
// ---------- SUM(bool) 这两个写法是 MySQL 特有的 ----------
//
// `SUM(account_id = 0)` 和 `SUM(stall_count > 0)` 依赖 MySQL 把布尔表达式
// 求值成 1/0 再求和 —— 也就是"计数满足条件的行"。这是 MySQL 的方言，
// 标准 SQL 要写成 `SUM(CASE WHEN ... THEN 1 ELSE 0 END)`。
//
// 本项目的 driver 是 go-sql-driver/mysql，不会换库，所以用方言没问题。
// 但它值得标注一句，因为**这两个写法在别的数据库上会直接报错**，
// 而报错信息（"Unknown column"或类型错误）不会指向真正的原因。
func (r *QoERepository) Totals(ctx context.Context, videoID uint, days int) (*totalsRow, error) {
	q := r.db.WithContext(ctx).Model(&QoEEvent{})
	q = r.windowClause(q, days)
	if videoID > 0 {
		q = q.Where("video_id = ?", videoID)
	}

	var row totalsRow
	err := q.Select(`
		COUNT(*) AS total,
		COALESCE(SUM(account_id = 0), 0) AS guests,
		COALESCE(SUM(stall_count > 0), 0) AS stalled,
		COALESCE(SUM(played_ms), 0) AS played_ms,
		COALESCE(SUM(stall_total_ms), 0) AS stall_ms,
		COALESCE(SUM(dropped_frames), 0) AS dropped,
		COALESCE(SUM(tier_switches), 0) AS switches
	`).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// startupRow 首帧分位数的扫描目标。
type startupRow struct {
	N   int64
	P50 int64
	P95 int64
	P99 int64
}

// StartupPercentiles 首帧时间的 P50 / P95 / P99。
//
// ---------- 为什么在 SQL 里算，不拉回来在 Go 里算 ----------
//
// 拉回全部 startup_ms 再排序，代码更直观，但传输量是 O(会话数) ——
// 而这张表只增不减，早晚会到"为了算三个数而传回十万个整数"的地步。
// 窗口函数让数据库只返回三个数。
//
// 用 CTE + ROW_NUMBER 而不是 PERCENT_RANK：后者返回的是**排名比例**（0~1 的小数），
// 要拿分位数还得反过来找"第一个 ≥ 0.95 的行"，中间那步容易写错。
// ROW_NUMBER 直接给出 1..N 的整数序号，`rn = CEIL(N * p)` 就是第 p 分位，**直白到不用注释**。
//
// CEIL 而不是 ROUND：N=10 时 0.95*10=9.5，CEIL→10（最大值），ROUND→10 也是最大值；
// 但 N=3 时 0.95*3=2.85，CEIL→3，ROUND→3。真正的差别在
// "宁可选偏后面那个"这个取向上 —— 分位数在长尾分布上宁可报高不报低。
//
// ⚠ 需要 MySQL 8.0+（CTE 和窗口函数都是 8.0 才有的）。本项目 docker 里是 mysql:8.0。
// 在 5.7 上这条会直接语法错误 —— 不是静默降级，会明确报错，所以不用额外防护。
func (r *QoERepository) StartupPercentiles(ctx context.Context, videoID uint, days int) (*startupRow, error) {
	inner := r.db.WithContext(ctx).Model(&QoEEvent{}).Where("startup_ms > 0")
	inner = r.windowClause(inner, days)
	if videoID > 0 {
		inner = inner.Where("video_id = ?", videoID)
	}

	// startup_ms > 0 是必需的过滤：没播成的会话（用户点了播放但立刻退出、
	// 或者直接报错）startup_ms 是 0。把 0 混进分位数里，
	// P50 会被拖到一个偏小的假值上 —— 而那些会话根本不是"首帧很快"。
	sql := `
		WITH ranked AS (
			SELECT startup_ms,
			       ROW_NUMBER() OVER (ORDER BY startup_ms) AS rn,
			       COUNT(*) OVER () AS cnt
			FROM qoe_events
			WHERE startup_ms > 0` + r.rawWindowSQL(videoID, days) + `
		)
		SELECT
			COALESCE(MAX(cnt), 0) AS n,
			COALESCE(MAX(CASE WHEN rn = CEIL(cnt * 0.50) THEN startup_ms END), 0) AS p50,
			COALESCE(MAX(CASE WHEN rn = CEIL(cnt * 0.95) THEN startup_ms END), 0) AS p95,
			COALESCE(MAX(CASE WHEN rn = CEIL(cnt * 0.99) THEN startup_ms END), 0) AS p99
		FROM ranked`

	var row startupRow
	if err := r.db.WithContext(ctx).Raw(sql, r.rawArgs(videoID, days)...).Scan(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// rawWindowSQL / rawArgs 给上面那条 Raw SQL 拼时间窗和 video 过滤。
//
// 为什么 Raw 这条不能像别的查询那样用链式 Where：它是一个 CTE，
// 过滤条件必须写在 **CTE 内部**（写在外面就变成了对结果集再过滤，
// 分位数的分母就错了 —— 那是最难发现的一类错：数字看着正常，只是全错）。
//
// 所以这里把条件和参数抽成一对函数，保证 SQL 片段和参数列表**永远同进同出**。
// 参数顺序：先 video_id 后 days，和拼接顺序一致 —— 反了不会报错，
// 只会静默地用一个错误的时间窗去筛，得出一个看起来合理的错误数字。
func (r *QoERepository) rawWindowSQL(videoID uint, days int) string {
	s := ""
	if videoID > 0 {
		s += " AND video_id = ?"
	}
	if days > 0 {
		s += " AND created_at >= ?"
	}
	return s
}

func (r *QoERepository) rawArgs(videoID uint, days int) []any {
	args := []any{}
	if videoID > 0 {
		args = append(args, videoID)
	}
	if days > 0 {
		args = append(args, time.Now().AddDate(0, 0, -days))
	}
	return args
}

// bitrateRow 码率均值的扫描目标。
type bitrateRow struct {
	N   int64
	Avg int64
}

// Bitrate 只统计**走 HLS 的会话**（avg_bitrate_kbps > 0）的平均码率。
//
// 把直传会话的 0 混进来会把均值拉到一个无意义的数上：假设 100 个会话里
// 90 个是直传（0）、10 个是 HLS 平均 1500 kbps，混着算出来是 150 kbps ——
// 这个数不对应任何东西，"平均 150 kbps"既不是码率也不是任何人的体验。
//
// 这也是为什么返回 N（参与计算的会话数）：N=3 的时候那个均值不该被当真。
func (r *QoERepository) Bitrate(ctx context.Context, videoID uint, days int) (*bitrateRow, error) {
	q := r.db.WithContext(ctx).Model(&QoEEvent{}).Where("avg_bitrate_kbps > 0")
	q = r.windowClause(q, days)
	if videoID > 0 {
		q = q.Where("video_id = ?", videoID)
	}

	var row bitrateRow
	err := q.Select(`
		COUNT(*) AS n,
		COALESCE(ROUND(AVG(avg_bitrate_kbps)), 0) AS avg
	`).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// bucketRow 分桶查询的扫描目标。
type bucketRow struct {
	Label string
	Count int64
}

// tierBuckets 四个固定桶的标签，顺序即展示顺序。
//
// **必须写死，不能用 ORDER BY 从数据里推** —— 见 entity.go 里 TierBucket 的说明：
// 跨时间对比时桶必须稳定。从数据里推的话，"今天最高的桶"和"上周最高的桶"
// 含义可能不同，两张看板并排放会得出错误结论。
//
// ⚠ **这里的标签和下面 Tiers() 那条 SQL 里的 CASE 边界是两份，必须一起改。**
// 改一边不改另一边不会报错：SQL 分出来的 label 在 got 里匹配不上，
// 那一桶静默变成 0 —— 表现是"某档永远没人"，而不是报错。
// 这是本次实现里唯一一处"两处必须同步但没有编译期保护"的地方，
// 特意把这个警告放在这个字面量上面，因为改的人一定会先看到它。
var tierBuckets = []string{
	"0~500 kbps",
	"500~1500 kbps",
	"1500~3000 kbps",
	"3000+ kbps",
}

// Tiers 码率分桶统计。**返回的永远是四个桶**，没有数据的桶补 0。
//
// 补 0 而不是省略，是为了让前端能直接渲染而不用做"桶缺失时补位"的逻辑 ——
// 那种逻辑一旦写错就是"柱状图少一根"，而少一根柱子比多一根 0 柱子危险得多。
//
// 用 CASE 分组而不是按区间多次查询：一次扫描出四行，而不是扫四遍。
func (r *QoERepository) Tiers(ctx context.Context, videoID uint, days int) ([]TierBucket, error) {
	q := r.db.WithContext(ctx).Model(&QoEEvent{}).Where("avg_bitrate_kbps > 0")
	q = r.windowClause(q, days)
	if videoID > 0 {
		q = q.Where("video_id = ?", videoID)
	}

	var rows []bucketRow
	err := q.Select(`
		CASE
			WHEN avg_bitrate_kbps < 500  THEN '0~500 kbps'
			WHEN avg_bitrate_kbps < 1500 THEN '500~1500 kbps'
			WHEN avg_bitrate_kbps < 3000 THEN '1500~3000 kbps'
			ELSE '3000+ kbps'
		END AS label,
		COUNT(*) AS count
	`).Group("label").Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	// 按固定顺序铺开并补 0（SQL 里 GROUP BY 只会返回有数据的桶）
	got := make(map[string]int64, len(rows))
	for _, r := range rows {
		got[r.Label] = r.Count
	}
	out := make([]TierBucket, 0, len(tierBuckets))
	for _, label := range tierBuckets {
		out = append(out, TierBucket{Label: label, Count: got[label]})
	}
	return out, nil
}
