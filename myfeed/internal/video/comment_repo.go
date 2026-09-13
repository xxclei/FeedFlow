package video

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// CommentRepository comments 表的读写。
//
// **这个 repo 的方法名和 LikeRepository 有一处刻意的不同：写方法一律带 Tx 后缀。**
// LikeRepository 提供了两套（Like 自开连接版 + LikeTx 必须传 tx 版），这里只给 Tx 版。
//
// 原因是阶段4 那条教训（见 like_repo.go 顶上的长注释）：
// **任何用 r.db 的方法，在 db.Transaction 闭包里被调用都会跑到事务外那条连接上，
// 立刻独立提交** —— 事务回滚了它也不回滚。
//
// LikeRepository 保留非 Tx 版是为了"独立调用者"（那时确实有）；但评论的每一个写
// 都在事务里（插评论 + 热度 +1 必须同生共死）。既然唯一的用法就是带 tx 进来，
// 那就**不提供**会走错连接的那个版本 —— 让类型系统来保证纪律，
// 而不是靠人记住"进事务了要用 Tx 那个"。
type CommentRepository struct {
	db *gorm.DB
}

func NewCommentRepository(db *gorm.DB) *CommentRepository {
	return &CommentRepository{db: db}
}

// Transaction 开事务的入口。和 LikeRepository.Transaction 同构：
// service 拿得到 tx，拿不到 db。
func (r *CommentRepository) Transaction(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return r.db.WithContext(ctx).Transaction(fn)
}

func (r *CommentRepository) CreateCommentTx(tx *gorm.DB, comment *Comment) error {
	return tx.Create(comment).Error
}

// DeleteCommentTx 按**主键**删（传入带 ID 的对象），返回本次是否真的删到了行。
//
// 返回值是 (bool, error) 而不是裸 error，理由和 LikeRepository.DeleteByVideoAndAccountTx
// 完全一样：**DELETE 影响 0 行不是错误**，GORM 返回的 err 是 nil。
// 不看 RowsAffected 的后果在阶段4 已经演示过一遍 —— 那边是"计数凭空变少"，
// 这边更严重：删评论现在要**顺带把视频热度 -1**，
// 两个并发请求各自 GetByID 成功、各自删、各自 -1，热度就被扣了两次。
//
// 原项目这里是 `func (r *CommentRepository) DeleteComment(ctx, comment) error`，
// 没看 RowsAffected。它当时没有热度 -1 所以看不出来，我们补了 -1，就非看不可了。
// 这是"给一个动作加副作用"会反过来收紧前一步契约的典型例子。
//
// 传 `&Comment{}` 而不带 ID 会删不动（主键为 0，GORM 会拒绝执行并返回
// ErrMissingWhereClause），所以调用方必须传 GetByID 拿回来的那个对象。
func (r *CommentRepository) DeleteCommentTx(tx *gorm.DB, comment *Comment) (bool, error) {
	res := tx.Delete(comment)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// GetAllComments 某个视频的全部评论，时间**升序**（评论区从旧到新）。
//
// 两个容易凭直觉写错的地方：
//
//	① 是 ASC 不是 DESC。列表页那几条流（最新/点赞榜/热榜）全是倒序，
//	   手一滑就抄成 DESC，评论区就会变成"最新的在最上面"，翻页方向反了。
//	② LIMIT 200 是硬上限，没有游标、没有分页 —— 第 201 条评论**永远拿不到**。
//	   这是原项目的形态（旧版文档说"不分页"已过时，上限是后加的）。
//	   想练阶段3 的游标可以把它改成 (created_at, id) 双键，但那要先改接口契约。
//
// 无评论时 Find 留下的是 nil 切片 → JSON 会序列化成 null → 前端 v-for 炸。
// 兜底放在 handler（nonNilComments），和 feed/like 保持在 HTTP 层兜同一条纪律。
func (r *CommentRepository) GetAllComments(ctx context.Context, videoID uint) ([]Comment, error) {
	var comments []Comment
	err := r.db.WithContext(ctx).
		Where("video_id = ?", videoID).
		Order("created_at asc").
		Limit(200).
		Find(&comments).Error
	return comments, err
}

// GetByID 约定：**ErrRecordNotFound → (nil, nil)**，调用方必须判 nil。
//
// 为什么不让它直接返回 gorm.ErrRecordNotFound：那样 service 里就得写
// `if errors.Is(err, gorm.ErrRecordNotFound)`，而 gorm 的错误一旦漏到
// apierror.ClassifyHTTPStatus 就会被翻译成 **404**。
// 这条路径上我们要的是 500 + "comment not found"（原项目行为），
// 所以让 repo 把"没找到"变成一个普通返回值，把"翻译成什么错误"的决定权留给 service。
//
// 注意这个约定是**危险**的：忘了判 nil 就是空指针 panic。
// 只有两处调用（Delete），都判了。
func (r *CommentRepository) GetByID(ctx context.Context, id uint) (*Comment, error) {
	var comment Comment
	if err := r.db.WithContext(ctx).First(&comment, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &comment, nil
}

// 原项目还多一个 IsExist(ctx, id) (bool, error)，是**死代码** —— service 里一次都没调
// （它需要评论内容，所以只能走 GetByID）。这里不抄：一个没人调的方法，下一个人会花
// 五分钟猜"它是不是给阶段9 的 Worker 预留的"，然后发现不是。
// 阶段9 的 Worker 要"存在性检查"时，它需要的也是 GetByID（要拿 videoID 才能改热度）。
