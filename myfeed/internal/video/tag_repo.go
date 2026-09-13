package video

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// maxTagNameRunes 对应 tags.name 的 varchar(100)。
//
// **MySQL 的 varchar(N) 里的 N 是字符数，不是字节数** —— 所以这里也必须按 rune 截，
// 按 byte 截会把一个中文标签在中间切开（而且 100 字节只放得下 33 个汉字，
// 会比列宽更早地砍掉合法输入）。
//
// 不截的后果不是"存进去一个长标签"，而是 FirstOrCreate 撞 1406
// (Data too long for column 'name') → 被兜成 500 → 用户看到"发布失败"却不知道
// 是因为标签太长。批量上传时一个手滑粘进来的长文本会让整批都发不出去。
const maxTagNameRunes = 100

// AttachTagsTx 在事务里把标签挂到视频上：找到（或建出）tag 行，再建关联行。
//
// 这段逻辑原先**内联在 VideoService.Publish 的事务闭包里**。抽出来的直接原因
// 是本轮多了一个标签来源（tag_names），两个来源要汇进同一条写入路径；
// 顺带解决了"没有可复用的 tx 版标签写入"这个分层瑕疵。
//
// 接收 tx 而不是自己开事务，也不是收 ctx：**它必须和视频插入在同一个事务里**，
// 否则会出现"视频发布成功但标签丢了"或反过来。
func (vr *VideoRepository) AttachTagsTx(tx *gorm.DB, videoID uint, names []string) error {
	for _, tagName := range names {
		tagID, err := findOrCreateTagTx(tx, tagName)
		if err != nil {
			return err
		}

		// 关联行用 OnConflict DoNothing，而不是先查后插、也不是捕 1062。
		//
		// 因为本轮给 video_tags 加了复合唯一索引，(视频,标签) 重复现在会**报错**，
		// 而"这条视频已经有这个标签了"完全不是一个错误 —— 它是幂等的正常结果。
		// 让数据库去判重比在 Go 里"查一次再插"可靠：后者在并发下本来就有窗口。
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&VideoTag{VideoID: videoID, TagID: tagID}).Error; err != nil {
			return err
		}
	}
	return nil
}

// findOrCreateTagTx 拿到标签的 id，不存在就建一个。
//
// FirstOrCreate 在**并发**下会撞 tags.name 的唯一索引：两个请求同时没查到，
// 于是同时插，其中一个必然 1062。这不是理论问题 —— 批量上传时同一批文件带同一个
// 标签是常态，而我们前台按 (文件级并发 2) 在跑，两个 publish 撞上同一个新标签
// 只需要时序凑巧。
//
// GORM 的 FirstOrCreate **不**替我们重试，所以这里显式做：
// 撞了 1062 就说明另一个事务刚刚把它建出来了，**重新查一次**即可
// （不是重试整个插入 —— 那个必然再撞一次）。
func findOrCreateTagTx(tx *gorm.DB, name string) (uint, error) {
	var tag Tag
	err := tx.Where("name = ?", name).FirstOrCreate(&tag, Tag{Name: name}).Error
	if err == nil {
		return tag.ID, nil
	}
	if !isDuplicateKey(err) {
		return 0, err
	}

	// 别人抢先建好了 → 重新查一次（不是重试整个插入，那个必然再撞一次）。
	//
	// **这里必须是锁定读（FOR UPDATE），写成普通 First 会查不到。** 这一点极不显眼：
	// InnoDB 默认的 REPEATABLE READ 下，普通 SELECT 读的是**本事务第一次读时**
	// 建立的快照，而对方那行是在那之后才提交的 —— 在我们这个快照里它**不存在**。
	// 于是普通 First 返回 ErrRecordNotFound，调用方拿到一个 0 号 tagID
	// （GORM 失败时只塞了 error，没动 tag.ID），紧接着插一条 video_tags(video_id, 0) ——
	// 这张表**没有外键约束**，MySQL 不会拦，于是一条脏关联静默写进去了，
	// 而且它长得和正常行一模一样。
	//
	// FOR UPDATE 是"当前读"：绕过快照去读最新已提交版本，并且会等对方的行锁释放，
	// 所以它读得到。代价是它拿了把行锁 —— 但下一句本来就是往这行上挂关联，锁是迟早要的。
	//
	// 只重试一次。第二次还撞就说明不是这个竞态（更可能是别的唯一键或数据问题），
	// 原样上抛让它变成 500，别把真问题吞进一个"重试就好"的壳里。
	var fresh Tag
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("name = ?", name).
		First(&fresh).Error; err != nil {
		return 0, fmt.Errorf("标签 %q 撞唯一键后回读失败: %w", name, err)
	}
	return fresh.ID, nil
}

// isDuplicateKey 是否为 1062 (ER_DUP_ENTRY)。
//
// 不用 gorm.ErrDuplicatedKey：那个需要 gorm.Config{TranslateError: true}，
// 而 db.go 里没开（开了会改变**全项目**所有 repo 拿到的错误形状，
// 为一个局部的重试去动全局配置，代价和收益不成比例）。
// 这里直接看 MySQL 驱动给的错误码，和 like_concurrency_test.go 里的判法一致。
func isDuplicateKey(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && myErr.Number == 1062
}

// normalizeTagNames 清洗显式传入的标签：去空白 → 去前导 # → 丢空 → 截长 → 去重。
//
// 为什么需要"去前导 #"：调用方（前端 chip 编辑器）已经剥过一层，但这里是
// **数据进入数据库的最后一道**，不该假设上游一定干净。用户直接打 API 时
// 会送 "日常" 还是 "#日常" 是不确定的，两种都应该work。
//
// 去重为什么要用 ToLower 当键 —— 这条是本轮最容易忽略的坑：
// tags.name 的排序规则是 utf8mb4_unicode_ci，**大小写不敏感**，
// 所以 "Vlog" 和 "vlog" 在数据库看来是**同一个标签**。
// 如果这里按原样（区分大小写）去重，["Vlog","vlog"] 会留下两项，
// 第二项的 FirstOrCreate 会命中第一项刚建的行、拿到同一个 tagID，
// 然后第二条 VideoTag 插进去 → 撞刚加的复合唯一索引 → **1062 → 整个发布回滚**。
// 也就是说：不去重这个大小写，唯一索引会把合法的发布搞挂。
//
// 需要说清的是 Go 的 ToLower 和 MySQL 的 CI 排序规则**不是**严格等价的
// （某些语言的变音、土耳其语的 I 等）。这里只挡住实际会遇到的情况，
// 残余情况由 AttachTagsTx 的 OnConflict DoNothing 兜底 —— 两层的分工要记住。
func normalizeTagNames(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))

	for _, raw := range names {
		name := strings.TrimSpace(raw)
		// 只剥前导的 #（一个或多个），中间和末尾的 # 是非法字符，交给下面的正则思路不管 ——
		// 标签名存的是**不含 # 的内容**（见 Tag.Name 的注释），所以前导 # 必须剥掉
		name = strings.TrimLeft(name, "#")
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		if r := []rune(name); len(r) > maxTagNameRunes {
			name = string(r[:maxTagNameRunes])
		}

		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}
