package video

import "time"

type Video struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	AuthorID    uint      `gorm:"index;not null" json:"author_id"`
	Username    string    `gorm:"type:varchar(255);not null" json:"username"`
	Title       string    `gorm:"type:varchar(255);not null" json:"title"`
	Description string    `gorm:"type:varchar(255);" json:"description,omitempty"`
	PlayURL     string    `gorm:"type:varchar(255);not null" json:"play_url"`
	CoverURL    string    `gorm:"type:varchar(255);not null" json:"cover_url"`
	CreateTime  time.Time `gorm:"autoCreateTime;index:idx_videos_create_time,sort:desc;index:idx_videos_popularity_time_id,priority:2,sort:desc" json:"create_time"`
	LikesCount  int64     `gorm:"column:likes_count;not null;default:0;index:idx_videos_likes_count_id,priority:1,sort:desc" json:"likes_count"`
	Popularity  int64     `gorm:"column:popularity;not null;default:0;index:idx_videos_popularity_time_id,priority:1,sort:desc" json:"popularity"`

	// ---------- 混合检索的向量回填状态（本轮新增） ----------
	//
	// **两个字段都带 `json:"-"`，这不是洁癖。**
	// /video/getDetail、/video/listByAuthorID、/video/publish 都是**直接把 Video
	// 结构体丢给 c.JSON** 的，不加这个标记，这两列会出现在每一个视频响应里，
	// 而前端 api/video.ts 的 VideoItem 接口里没有它们 —— 类型和实际响应悄悄漂移，
	// TS 那边永远不会报错，等哪天有人按响应写代码才发现。
	//
	// 为什么向量本体不在这里（而是存 Redis）：Redis 在本项目里是**可选降级**的，
	// 向量检索跟着它一起降级正好合适；而 MySQL 是唯一真相源，不该为搜索这个
	// 可降级的功能背上 127×4KB 的二进制列。下一段说的这两列**只是元数据**。
	//
	// 这两列存在的唯一理由：让"哪些视频没嵌 / 嵌的是哪个模型"变成一个能用 SQL
	// 直接问出来的问题。**换 embedding 模型 = 全量重嵌**（re-embedding），
	// 这是向量检索真实的运营痛点 —— 模型名不落表就无解，而无解的表现是
	// 新旧向量混在同一个空间里，余弦值全是垃圾**且不报错**。
	//
	// ---------- 本轮状态：这两列**没有写入者** ----------
	//
	// 向量那一路（Redis Stack + Ollama + 回填命令）本轮先跳过，
	// 所以 AutoMigrate 会把这两列建出来，但永远是 NULL。
	// 留着它们而不是删掉，是因为**删了就丢了这个取舍的记录**：
	// 接向量时最容易漏掉的恰恰是"模型名要落表"这件事（漏了的后果如上，
	// 不报错、只是搜索慢慢变垃圾）。代价只是两列可空的 varchar/datetime。
	EmbeddingModel *string    `gorm:"type:varchar(64)" json:"-"`
	EmbeddedAt     *time.Time `json:"-"`
}

type PublishVideoRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	PlayURL     string `json:"play_url"`
	CoverURL    string `json:"cover_url"`

	// TagNames 是本轮新增的**显式标签**（批量上传时统一设置的那种）。
	//
	// 它和描述文本里手写的 #xxx 是**并集**关系，不是替代：
	// 服务端落库时取 ExtractTags(title+" "+description) ∪ tag_names。
	// 因为是并集，所有既有行为原样保留 —— 单独发布页照旧靠 TagInput 往描述里写 #标签，
	// 一个字段都不改。
	//
	// 为什么不让批量上传继续往 description 里塞 "#vlog #日常"（本轮之前就是这么干的）：
	//   1. 标签成了描述的一部分，吃 varchar(255) 的额度，和真正的描述互相挤；
	//   2. 那个输入框是个裸 input，用户打「日常 vlog」不带 # 就**静默零标签** ——
	//      没有报错、没有提示，只有事后发现标签没打上；
	//   3. 语义上标签不是描述。这一点在搜索里会立刻显形：描述是用来匹配的正文，
	//      标签是分类元数据。
	TagNames []string `json:"tag_names"`
}

type DeleteVideoRequest struct {
	ID uint `json:"id"`
}

// maxBatchDeleteIDs 一次批量删除的 id 数上限。
//
// 这条限制的存在理由是**请求体大小 + 事务持有时间**，不是"业务上不该一次删这么多"：
// 每个 id 都要进一个 IN 列表、并且整个删除跑在一个事务里，
// 几千个 id 会让事务敞着的时间从毫秒变成秒级，期间占着行锁。
// 100 是个远大于真实用量（谁会在一个页面上勾 100 个视频）又足够小的值。
const maxBatchDeleteIDs = 100

// DeleteBatchRequest 批量删除。
//
// IDs 里混进"不属于我"或"不存在"的 id 是**正常输入**，不是错误 ——
// 前端的多选列表可能是旧的（另一个标签页删过了），这种情况下正确的行为是
// 删掉能删的、如实报告跳过了哪些，而不是整个请求失败。
// 所以校验只挡"明显坏掉的请求"（空、过多），归属问题留给 service 按 id 回报。
type DeleteBatchRequest struct {
	IDs []uint `json:"ids"`
}

// DeleteBatchResponse 部分成功的如实报告。
//
// **不能只回一个 deleted 计数**：前端需要知道"我勾的这 3 条里，是哪 1 条没删掉"，
// 才能把它留在选中态里让用户重试，而不是笼统提示一句"删除成功"然后
// 用户刷新发现还有一条在 —— 那种情况下用户只会认为删除功能是坏的。
type DeleteBatchResponse struct {
	Deleted    int    `json:"deleted"`
	DeletedIDs []uint `json:"deleted_ids"`
	SkippedIDs []uint `json:"skipped_ids"`
}

type ListByAuthorIDRequest struct {
	AuthorID uint `json:"author_id"`
}

type GetDetailRequest struct {
	ID uint `json:"id"`
}

type UpdateLikesCountRequest struct {
	ID         uint  `json:"id"`
	LikesCount int64 `json:"likes_count"`
}

// OutboxMsg 事件信的存根：publish 事务里写入 pending，
// 阶段9 的 Poller 把它投递到 MQ 后才删掉（MySQL 是唯一真相源）
type OutboxMsg struct {
	ID         uint      `gorm:"primaryKey"`
	VideoID    uint      `gorm:"index"`
	EventType  string    `gorm:"type:varchar(50)"`
	CreateTime time.Time `gorm:"autoCreateTime"`
	Status     string    `gorm:"type:varchar(50);index"`
}
