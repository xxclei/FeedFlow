package social

import (
	"context"

	"myfeed/internal/account"

	"gorm.io/gorm"
)

// SocialRepository socials 表的读写。
//
// **这个 repo 和 LikeRepository/CommentRepository 有一个本质区别：写方法不需要
// 事务，也不需要看 RowsAffected。** 理由是本模块最值得记的一条：
//
//	关注关系的写入**没有副作用**。它不维护任何冗余计数 ——
//	粉丝数/关注数是每次 SELECT COUNT(*) 现算的（见 CountFollowers）。
//
// 所以：
//
//	Follow   —— 一次 INSERT。并发重复由唯一索引拦（1062），不需要事务包住别的东西。
//	Unfollow —— 一次 DELETE。**删 0 行真的无所谓**：没有计数要跟着减，
//	            重复删除的结果就是"没关注"，状态自洽。
//
// 对比一下就会明白 RowsAffected 纪律到底在保护什么：
//
//	阶段4 Unlike   删 0 行必须报错，因为后面跟着 likes_count -1 ——
//	               不看就会把计数扣两遍
//	阶段5 DeleteComment  同样，后面跟着 popularity -1（我们补的那步）
//	阶段6 Unfollow 删 0 行无所谓 —— 后面什么都跟着
//
// 结论：**看 RowsAffected 不是 DELETE 的通用纪律，是"有副作用跟着"的纪律。**
// 记成前者会让人在不必要的地方写防御代码，记成后者才知道该在哪里停下来检查。
//
// 顺带解释"为什么 social 敢用实时 COUNT 而 like 不敢":
// 冗余计数是为了读热点付的写复杂度税。点赞数要跟着每一条 feed 卡片出现，是热点；
// 粉丝数只在个人主页出现一次，不是。**没有热点，就不交这笔税** ——
// 于是 Unfollow 连 RowsAffected 都不用看，整个写路径塌缩成一行 SQL。
type SocialRepository struct {
	db *gorm.DB
}

func NewSocialRepository(db *gorm.DB) *SocialRepository {
	return &SocialRepository{db: db}
}

// Follow 插入一条关注关系。
// 并发重复关注会撞唯一索引 → MySQL 1062 → 裸着变成 500。
// 原项目就是这样（文档 常见の坑 里列为可改进项：翻译成 "already followed" 或 409/200）。
// 本阶段保留原样，和阶段4 点赞的 isDupKey 翻译做成对照。
func (r *SocialRepository) Follow(ctx context.Context, social *Social) error {
	return r.db.WithContext(ctx).Create(social).Error
}

// Unfollow 按 (follower_id, vlogger_id) 成对删除。
//
// 传入的 social 对象**只需要这两个字段**，ID 用不上 —— 和 DeleteCommentTx
// 按主键删不同：那条边没有主键可供调用方持有（前端只知道自己和对方的 accountID），
// 所以条件删除是唯一可行的形态。
//
// 返回裸 error，不返回 RowsAffected 布尔 —— 理由见本文件顶上那段。
func (r *SocialRepository) Unfollow(ctx context.Context, social *Social) error {
	return r.db.WithContext(ctx).
		Where("follower_id = ? AND vlogger_id = ?", social.FollowerID, social.VloggerID).
		Delete(&Social{}).Error
}

// IsFollowed 预检用（Follow/Unfollow 在写之前各调一次）。
// COUNT > 0 而不是 First + ErrRecordNotFound —— 这里不关心具体是哪一行。
func (r *SocialRepository) IsFollowed(ctx context.Context, social *Social) (bool, error) {
	var count int64
	if err := r.db.WithContext(ctx).
		Model(&Social{}).
		Where("follower_id = ? AND vlogger_id = ?", social.FollowerID, social.VloggerID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetAllFollowers 粉丝列表：**两段查询** —— 先从 socials 拿 ID 列表，再 IN 查 accounts。
//
// 三个必须照抄的细节：
//
//	① Limit(200) 硬上限（和评论的 200 同款）：第 201 个粉丝**永远拿不到**，
//	   且响应里的 follower_count 是**真实总数**——所以会出现
//	   "follower_count = 350，但列表只有 200 条"这种看似矛盾、实际正确的情况。
//	② ID 列表为空**提前 return 空切片**：不能拿着空 IN 去查下一句。
//	   空 IN 在 SQL 里是 `IN (NULL)`，匹配不到任何行（不会返回全表），
//	   但它白白多花一次往返，而且如果哪天有人把 IN 换成别的写法就是全表事故。
//	③ 两段都**没有 ORDER BY**，所以顺序 = 存储引擎给的顺序。
//
// 关于 ③ 有一个原项目没说破的事实：**第二段查询的 IN 不保证保持第一段的顺序**
// （MySQL 的 `WHERE id IN (...)` 不按 IN 列表排序）。第一段的 Limit(200) 是在
// **没有 ORDER BY** 的情况下取的 200 行，本身就不保证是"最早关注的 200 个"。
// 所以文档说的"按主键序（≈关注先后）"是近似，不是保证 —— 别在它上面建任何逻辑。
// 想要真顺序，得加 created_at 列（见 entity.go 的说明）。
//
// 另一种写法是 JOIN（一条 SQL，无 N+1 风险）：
//
//	SELECT accounts.* FROM socials
//	  JOIN accounts ON accounts.id = socials.follower_id
//	 WHERE socials.vlogger_id = ? LIMIT 200
//
// 两条 SQL 变一条，代价是 social 包要多认识 accounts 表的列（这里已经认识了，
// 因为返回类型就是 *account.Account）。两种都写得出来就够了，本阶段按原项目的两段式。
func (r *SocialRepository) GetAllFollowers(ctx context.Context, vloggerID uint) ([]*account.Account, error) {
	var relations []Social
	if err := r.db.WithContext(ctx).
		Model(&Social{}).
		Where("vlogger_id = ?", vloggerID).
		Limit(200).
		Find(&relations).Error; err != nil {
		return nil, err
	}

	// make(..., 0, len) 而不是 var：容量预分配，append 一次不扩容
	followerIDs := make([]uint, 0, len(relations))
	for _, rel := range relations {
		followerIDs = append(followerIDs, rel.FollowerID)
	}
	// ② 的落点：空列表提前返回，别拿空 IN 去查
	if len(followerIDs) == 0 {
		return []*account.Account{}, nil
	}

	var followers []*account.Account
	if err := r.db.WithContext(ctx).
		Model(&account.Account{}).
		Where("id IN ?", followerIDs).
		Find(&followers).Error; err != nil {
		return nil, err
	}
	return followers, nil
}

// GetAllVloggers 关注列表：GetAllFollowers 的镜像实现（交换两列的角色）。
// 走 idx_social_follower_vlogger 的最左前缀，不用回表到 idx_social_vlogger。
func (r *SocialRepository) GetAllVloggers(ctx context.Context, followerID uint) ([]*account.Account, error) {
	var relations []Social
	if err := r.db.WithContext(ctx).
		Model(&Social{}).
		Where("follower_id = ?", followerID).
		Limit(200).
		Find(&relations).Error; err != nil {
		return nil, err
	}

	vloggerIDs := make([]uint, 0, len(relations))
	for _, rel := range relations {
		vloggerIDs = append(vloggerIDs, rel.VloggerID)
	}
	if len(vloggerIDs) == 0 {
		return []*account.Account{}, nil
	}

	var vloggers []*account.Account
	if err := r.db.WithContext(ctx).
		Model(&account.Account{}).
		Where("id IN ?", vloggerIDs).
		Find(&vloggers).Error; err != nil {
		return nil, err
	}
	return vloggers, nil
}

// CountFollowers "谁关注了我"的总数。**每次现算**，走 idx_social_vlogger。
//
// 和 likes_count 冗余列的对照是阶段6 的核心实验：
//
//	             likes_count 冗余列        粉丝数实时 COUNT
//	写的时候      每次点赞/取消各 UPDATE      什么都不做
//	读的时候      O(1) 读一行                COUNT(*) 扫索引
//	一致性风险     有（增量维护就会漂移）        零
//	选它的理由     读极频繁（每条 feed 卡片）   读稀疏（只在个人主页）
//
// 更关键的是**维护成本不对称**：关注是双向的，如果粉丝数/关注数都落冗余列，
// 那么一次 follow/unfollow 要维护**两个方向的两个计数**（follower 的 vlogger_count
// 和被关注者的 follower_count），还要担心"关注成功但计数没改"的不一致。
// 换来的只是省下一次主页访问的 COUNT —— 这笔账算不过来。
func (r *SocialRepository) CountFollowers(ctx context.Context, vloggerID uint) (int64, error) {
	var count int64
	if err := r.db.WithContext(ctx).Model(&Social{}).
		Where("vlogger_id = ?", vloggerID).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func (r *SocialRepository) CountVloggers(ctx context.Context, followerID uint) (int64, error) {
	var count int64
	if err := r.db.WithContext(ctx).Model(&Social{}).
		Where("follower_id = ?", followerID).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}
