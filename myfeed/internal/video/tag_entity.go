package video

import "regexp"

type Tag struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	Name string `gorm:"uniqueIndex;type:varchar(100);not null" json:"name"`
}

// VideoTag 多对多关联表：没有外键约束，两端的存在性由 service 校验
type VideoTag struct {
	ID      uint `gorm:"primaryKey"`
	VideoID uint `gorm:"index;not null"`
	TagID   uint `gorm:"index;not null"`
}

// tagRegex 从描述文本里抽 #标签：支持中文/字母/数字/下划线
var tagRegex = regexp.MustCompile(`#([\p{L}\p{N}_]+)`)

// ExtractTags 抽出并去重 —— 用户写 "#日常 #vlog #日常" 只得 ["日常","vlog"]
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
