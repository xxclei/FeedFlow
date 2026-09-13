package video

import "regexp"

type Tag struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	Name string `gorm:"uniqueIndex;type:varchar(100);not null" json:"name"`
}

// VideoTag 多对多关联表：没有外键约束，两端的存在性由 service 校验
//
// 本轮（批量标签）加上了**复合唯一索引** (video_id, tag_id)。
// 在这之前它只有两个各自独立的单列索引，也就是说**同一个 (视频,标签) 对可以插进去 N 次**
// 而不报错 —— 批次里重复的标签、或将来任何一次重试，都会静默攒出重复行，
// 而 ListByTag 是 JOIN 出来的，重复关联会让同一条视频在标签流里出现多次。
//
// 加这个索引之前**必须先清理历史重复行**，否则 AutoMigrate 会失败：
//
//	SELECT video_id, tag_id, COUNT(*) c FROM video_tags GROUP BY 1,2 HAVING c>1;
//
// 两列上写**同名**的 uniqueIndex，GORM 才会把它们合成一个复合索引；
// 名字打错一个字符就变成两个各自唯一的单列索引，意思是"一条视频只能有一个标签"
// 和"一个标签只能属于一条视频"，而且插入第二条时才炸。
// 和阶段4 的 likes、阶段6 的 socials 是同一个坑，这次是第三次踩。
//
// ---------- 那三个索引：两个留着，第三个是冗余但故意留的 ----------
//
// 加完复合索引后 video_tags 上会有**三个**索引，其中 idx_video_tags_video_id
// 严格来说是冗余的（它是复合索引的最左前缀）。**但不要把它去掉**，理由是分叉：
// AutoMigrate 只会为"模型里声明过的"索引建，**从不删**它不认识的索引
// （gorm/migrator/migrator.go，只看 ParseIndexes 的结果）。
// 所以把 VideoID 上的 index tag 删掉的效果是：**已有库上那个索引还在，新建的库上没有** ——
// 同一个版本的应用，两套 schema。这类分叉平时无害，等到某天在别的机器上
// 复现不出一个"跟索引有关的"问题时才会想起来。
// 冗余索引留着的先例在阶段6 已经有（idx_social_follower），保持一致。
//
// TagID 的单列索引则是**真的**必需，不是冗余：它在复合索引的第二位，
// 最左前缀用不上它，而 ListByTag 的 JOIN 要的正是"按 tag_id 找 video_id"这个方向。
type VideoTag struct {
	ID      uint `gorm:"primaryKey"`
	VideoID uint `gorm:"uniqueIndex:idx_video_tag;index;not null"`
	TagID   uint `gorm:"uniqueIndex:idx_video_tag;index;not null"`
}

// tagRegex 从描述文本里抽 #标签：支持中文/字母/数字/下划线
var tagRegex = regexp.MustCompile(`#([\p{L}\p{N}_]+)`)

// ExtractTags 抽出并去重 —— 用户写 "#日常 #vlog #日常" 只得 ["日常","vlog"]
//
// 这是**标签的两个来源之一**（另一个是 PublishVideoRequest.tag_names，本轮新增）。
// 两者在 service 里取并集，不是二选一 —— 所以这个函数的行为变了不会破坏
// 任何既有路径，这正是不动它、只加一路的原因。
func ExtractTags(text string) []string {
	matches := tagRegex.FindAllStringSubmatch(text, -1)
	seen := make(map[string]bool)
	var tags []string
	for _, m := range matches {
		tag := m[1]
		if !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	return tags
}
