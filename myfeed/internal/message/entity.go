// Package message 私信模块（阶段12），全项目**最后**一个业务模块。
//
// 它是最标准的"四件套最小练习"：一张表、两个接口、没有缓存、没有 MQ、
// 没有并发竞争。写在阶段 12 是刻意的 —— 前面十一个阶段把难的东西都见过了，
// 这里用来确认"一个最普通的模块长什么样"。
//
// 和原项目的一处结构差异：原项目把 entity + repo + service + handler 全挤在
// 两个文件里（`entity.go` + `handler.go`），`Repository`/`Service` 只是 handler.go
// 里的瘦壳，**handler 甚至直接摸 `service.repo`**。本项目按约定拆成四个文件 ——
// 拆完就已经比原项目规范，这也说明"四件套"这个形式本身没有额外成本。
package message

import "time"

// Message 一条私信。
//
// 表名：GORM 按结构体名复数推导 → `messages`。**不是** message（保留字风险）。
//
// ---------- 为什么没有"会话表" ----------
//
// 只有一张消息表，会话是**推算**出来的：A 和 B 的会话 =
// `(from=A and to=B) or (from=B and to=A)` 按时间倒序。这是最省表的设计，
// 代价是每次拉历史都要这个 OR 查询 —— 所以索引必须**两个方向各建一个**
// （见下面两列的说明）。
//
// 换成"会话表 + 消息表"的形态会多一张表、多一次 join，好处是"最近联系人列表"
// 可以直接从会话表读，不用像现在这样从前端拼（见下方 §前端怎么拿联系人）。
type Message struct {
	ID uint `gorm:"primaryKey" json:"id"`

	// 两个方向的索引，缺一不可 —— 这是本表唯一值得细看的地方。
	//
	// 会话查询的 WHERE 是 `(from=? and to=?) OR (from=? and to=?)`，MySQL 对 OR
	// 的处理是**拆成两个子查询再 UNION**，于是它需要：
	//   子查询① 用 (from_id, to_id) 定位  → 走 idx_message_from
	//   子查询② 用 (to_id, from_id) 定位  → 走 idx_message_to
	//
	// 只建一个方向的索引，另一半就退化成全表扫。**双向查询要双向索引** ——
	// 和 socials 表那条 idx_social_vlogger 是同一个道理（那里不建也只是慢，
	// 这里不建是 OR 的两条腿断一条，慢得更明显）。
	//
	// 注意这两个是**单列**索引，不是复合的。做成 (from_id, to_id) 复合索引
	// 会更好（覆盖整个子查询条件），原项目是单列，本项目照抄 ——
	// 数据量到百万级再改，现在改了没有可观测收益。
	FromID uint `gorm:"index:idx_message_from;not null" json:"from_id"`
	ToID   uint `gorm:"index:idx_message_to;not null" json:"to_id"`

	// type:text 而不是 varchar：私信正文长度不可预期，原项目如此。
	// 后果是**不能给它建普通索引**（前缀索引可以），但私信不需要按内容检索。
	Content string `gorm:"type:text;not null" json:"content"`

	// IsRead 在**原项目里是死字段** —— 定义了但没有任何接口写它，永远是 false。
	// 本项目**照原样保留，也不写它**，理由：
	//
	//   要让它活过来，缺的不是一行 UPDATE，而是**一整套语义**：
	//   什么时候算已读？（打开聊天窗？窗口在前台时？）谁来标？（读的人在客户端，
	//   但要改的是对方发的那些行）批量标还是逐条标？拉历史时顺手标算不算
	//   隐式写？—— 每一个问题都有两三种合理答案，选错一种就要回滚数据。
	//
	// 这与"notification 的 markRead"形成对照：那里有明确的 UI 动作（点"全部已读"），
	// 语义是确定的，所以能实现。**字段能存不等于语义已定。**
	// 想要的话见文件末尾的"怎么让它活过来"。
	IsRead bool `gorm:"default:false" json:"is_read"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
}

// 前端怎么拿联系人：后端**没有**"联系人列表"接口（原项目也没有），
// 前端用 social 模块的 getAllVloggers（我关注的）∪ getAllFollowers（我的粉丝）
// 再去掉自己拼出来。
//
// 为什么不加一个 `POST /message/contacts`：那需要一个
// `SELECT DISTINCT ... FROM messages WHERE from=? OR to=?` 的查询，
// 但它给出的只是"聊过天的人"，而产品要的通常是"能聊天的人"（= 关注关系）。
// 两者不等价，加了这个接口反而要回答"没聊过的关注对象算不算联系人"。
// 从前端拼是**先有产品语义再找数据**，加接口是**先有数据再编语义**。

// SendMessageRequest 发送者**不在请求体里** —— 只从 JWT 取。
// 让客户端能传 from_id 等于让任何人冒充任何人发私信，和评论的 author_id 同一条纪律。
type SendMessageRequest struct {
	ToID    uint   `json:"to_id"`
	Content string `json:"content"`
}

type ListMessagesRequest struct {
	// PeerID 是**对方**的账号 ID，"我"从 JWT 取。
	// 所以这个接口天然是"我的视角"，不存在"偷看别人会话"的入口 ——
	// 想偷看得先拿到别人的 token。
	PeerID uint `json:"peer_id"`
}

// ListMessagesResponse 的 Messages 在 Go 里可能是 nil（空会话），
// 序列化出去就是 `null`。**handler 里必须兜成 `[]Message{}`** ——
// 前端拿到 null 直接 `.map` 就是 TypeError。
// 这个坑在 social 的 followers 列表、like 的 videos 列表上都踩过一遍了。
type ListMessagesResponse struct {
	Messages []Message `json:"messages"`
}
