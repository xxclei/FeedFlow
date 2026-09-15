package message

import (
	"context"

	"gorm.io/gorm"
)

// MessageRepository messages 表读写。**两个方法，都是一行 SQL。**
//
// 这是全项目最薄的 repo，值得记下来的恰恰是"它为什么能这么薄"：
//
//	没有事务    —— Create 是单表单行，没有跟着它的冗余计数要更新。
//	              （对照 VideoRepository.Publish：视频 + 标签 + outbox 三张表，
//	               必须包事务。）
//	没有缓存    —— 私信是**私有数据**，命中率天然极低（只有对话双方会读），
//	               缓存它等于给每个用户开一个只被读过一两次的 key。
//	               这和 video/getDetail（所有人读同一条）是完全相反的访问分布。
//	没有 RowsAffected 纪律 —— 这里根本没有 UPDATE/DELETE。
//
// **"薄"不是偷懒，是没有复杂度可管。** 如果一个模块写了三个月还是很薄，
// 该问的不是"是不是漏了什么"，而是"这个模块本来就不需要那些东西"。
type MessageRepository struct {
	db *gorm.DB
}

func NewMessageRepository(db *gorm.DB) *MessageRepository {
	return &MessageRepository{db: db}
}

// Send 落库一条私信。
//
// 不需要处理 1062（没有唯一索引）、不需要处理死锁（单行插入）、
// 不需要看 RowsAffected（Insert 要么成功要么报错）。
//
// 失败也只可能是三类：连接断了、表不存在、字段超长/非空约束。
// 全部裸着往上抛 —— 它们是**真故障**，没有一种是业务语义（对比 social 的
// "already followed"，那是业务语义，所以值得翻译）。
//
// **注意 Create 之后 msg.ID 会被 GORM 回填。** 这正是文档要求
// `/message/send` 返回"落库后的 Message 对象"的实现基础 ——
// 不是返回 `{"message":"ok"}`，而是把带 id / created_at 的完整对象还给客户端。
// 前端拿到 id 就能直接塞进消息列表，不用重新拉一次会话。
func (r *MessageRepository) Send(ctx context.Context, msg *Message) error {
	return r.db.WithContext(ctx).Create(msg).Error
}

// List 拉取"我和某人"的双向会话，最近 50 条，按时间倒序。
//
// ---------- 那条 OR 一定要看懂 ----------
//
//	(from_id=? AND to_id=?) OR (from_id=? AND to_id=?)
//	 └───── 我发给他的 ─────┘ └───── 他发给我的 ─────┘
//
// 参数传了两次：self, peer, peer, self。**顺序反了不会报错**，
// 它会静默返回一个"只有单向"的会话 —— 表现是"我发的消息他那边看不到"，
// 或者反过来。这类 bug 的排查成本极高，因为两边各自看都是"有数据的"。
// 写这种四参数 OR 时，最好的习惯是**把 self 都放奇数位、peer 都放偶数位**
// （和上面的 SQL 形状对齐），不要凭记忆摆。
//
// ---------- 为什么是双向查询而不是"两个列表合并" ----------
//
// 两次单查询再在 Go 里归并，也可以，但要处理"合并后凑不满 50 条"的边界
// （一边 30 条一边 30 条，合起来 60 > 50，谁砍？砍完顺序还要重排）。
// 一次双向查询让**数据库负责"取最近 50 条"这个语义**，边界天然正确。
//
// ---------- 索引：实测出来的结论，和"想当然"不一样 ----------
//
// **预期**：MySQL 对跨列 OR 会做 index_merge(union)，把 OR 拆成两个子查询
// 各走各的索引再合并 —— 这就是 entity.go 里那两个方向索引缺一不可的理由。
//
// **实测**（本机 65 行数据）：
//
//	EXPLAIN SELECT ... WHERE (from_id=6 AND to_id=7) OR (from_id=7 AND to_id=6)
//	          ORDER BY created_at DESC LIMIT 50
//	  type: ALL          ← 全表扫
//	  possible_keys: idx_message_from, idx_message_to   ← 两个索引都认识
//	  key: NULL          ← 但一个都没用
//	  Extra: Using where; Using filesort
//
// 加上 FORCE INDEX(idx_message_from, idx_message_to) 之后变成
// `type: range, key: idx_message_from` —— **仍然不是 index_merge**，
// 只用了 OR 两条腿里的一条，另一条退化成 filter。
// （65 行时优化器算出来"扫全表更便宜"，这是它该做的判断，不是 bug。）
//
// 所以这里必须诚实地写清楚三件事：
//
//	① 那两个索引的**作用是"让 index_merge 成为一种可能"**，不是"保证被用到"。
//	   possible_keys 里出现它们 = 结构对了；key 是 NULL = 优化器选择不用。
//	   这两件事要分开看，`show index from messages` 验的是前者。
//	② **在几百行的表上，你验证不出索引有没有用。** 本项目的 messages 表
//	   永远到不了十万行（个人私信量级），所以真实情况就是**全表扫 + filesort**——
//	   而这是完全可接受的。**不要为了一个在 65 行上等价、在 10 万行上才显形的
//	   差异去改结构。** 想真验证，得先灌十万行假数据再 EXPLAIN。
//	③ Order + Limit 无论如何都不走索引（Extra 里的 Using filesort）。
//	   想让排序也走索引，得把索引升级成复合的：
//	       idx_message_from (from_id, to_id, created_at)
//	       idx_message_to   (to_id, from_id, created_at)
//	   这样每个子查询能"边走索引边取前 50 条"（索引本身有序），合并后只需归并。
//
// 本项目**不升级**：换索引要重建表，而收益在当前量级下是 0。
// **知道升级路径存在，比现在就升级更值钱。**
//
// ---------- 为什么硬编码 50，不带分页参数 ----------
//
// 原项目如此。代价是**私信没有历史翻页** —— 超过 50 条就只能靠 UI 上的
// "加载更多"，而那个按钮没有后端支持。
//
// 不加 limit 参数的原因（值得记）：加了 limit 参数就必须加游标
// （before_id 或 before_created_at），否则翻页会重复/漏 —— 见阶段3 游标分页
// 里踩过的那套。**一个"加个参数"的改动实际是"加一套分页协议"**，
// 所以它不该顺手做。要做就照 feed/listByFollowing 的形状做完整。
func (r *MessageRepository) List(ctx context.Context, selfID, peerID uint) ([]Message, error) {
	var msgs []Message
	if err := r.db.WithContext(ctx).
		Where("(from_id = ? AND to_id = ?) OR (from_id = ? AND to_id = ?)", selfID, peerID, peerID, selfID).
		Order("created_at DESC").
		Limit(50).
		Find(&msgs).Error; err != nil {
		return nil, err
	}
	// ⚠ 空结果时 msgs 是 **nil 切片**，不是空切片。
	// GORM 的 Find 在有结果时才 append，没结果时它就保持 var 声明时的 nil。
	// 兜底放在 handler（那里才知道"要序列化给对方"这件事），这里原样返回。
	return msgs, nil
}
