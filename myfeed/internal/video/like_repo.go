package video

import (
	"context"
	"errors"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// LikeRepository likes 表的读写。本模块的写热点都从这里过。
//
// 事务边界的分工：**Service 决定"开不开事务、做哪几步"，Repo 只负责 SQL**。
// 所以方法名分三类，一眼能对上：
//
//	Transaction                        —— 开事务的入口（service 拿得到 tx，拿不到 db）
//	Like / DeleteByVideoAndAccount     —— 自开连接的非事务版（独立调用者用）
//	LikeTx / DeleteByVideoAndAccountTx —— 必须传 tx，在调用方的事务里跑
//
// **为什么非要有 Tx 版本**：任何用 lr.db 的方法在 db.Transaction 闭包里被调用，
// 都会跑到事务外的那条连接上**立刻独立提交** —— 事务回滚了它也不回滚。
//
// 逃跑要咬人，必须满足一个条件：**失败的那一步排在逃逸的写入之后**。
// 计数先独立提交了、后面的步骤再失败并触发回滚，才会留下
// "likes 表干干净净、likes_count 已经 +1"这种毫无痕迹的错数据。
//
// 请注意当前 service 的顺序恰好是"先插流水、后改计数"，失败（1062）排在计数之前，
// 所以这一版**碰巧是安全的**。那是运气，不是设计：阶段9 要在计数之后投递 MQ 消息，
// 那一刻顺序就变了。把 tx 做成显式参数，是让类型系统替我们记住这条纪律，
// 而不是靠人记住。
//
// 这个坑有一个确定性复现，见 like_concurrency_test.go 的
// TestTransactionEscape_DemonstratesDrift（两个子测试只差一个函数名）。
type LikeRepository struct {
	db *gorm.DB
}

func NewLikeRepository(db *gorm.DB) *LikeRepository {
	return &LikeRepository{db: db}
}

// Transaction 把"开事务"包一层，让 Service 拿到 tx 却拿不到 db。
//
// 为什么不干脆暴露 db（或者让 service 直接摸 lr.db，同包是能编译的）：
// 那样上面那条纪律就没人守了 —— 闭包里 `lr.db` 和 `tx` 一样合法，写错不报错。
// 这里挡一道，闭包里除了 tx 没别的可用。
func (lr *LikeRepository) Transaction(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return lr.db.WithContext(ctx).Transaction(fn)
}

// ---------- 写：点赞 ----------

// Like 非事务版：只插流水，不动计数。
// 本阶段 service **不走它**（计数必须和插入同生共死），保留是为了对齐原项目的 repo 表面。
func (lr *LikeRepository) Like(ctx context.Context, like *Like) error {
	return lr.LikeTx(lr.db.WithContext(ctx), like)
}

// LikeTx 插一条点赞流水。1062（唯一键冲突）原样抛出，由调用方翻译成业务错误 ——
// repo 不替上层决定"撞键意味着什么"。
func (lr *LikeRepository) LikeTx(tx *gorm.DB, like *Like) error {
	return tx.Create(like).Error
}

// LikeIgnoreDuplicate 幂等版：撞唯一键**不算错误**，用 created 告诉你"这次到底插进去了没有"。
//
// 本阶段没有调用者 —— 它和 DeleteByVideoAndAccount 是给阶段9 的 Worker 准备的。
// 那时消息是 at-least-once（同一条点赞事件可能投递两次），Worker 靠这个返回值决定
// "要不要动计数"：created == false → 流水早就有了，计数绝不能再加。
//
// 把"这个副作用到底发生了没有"折叠成一个 bool，是幂等最省事的形态 ——
// 调用方不需要知道 1062、不需要去重表，只需要看这个 bool。
func (lr *LikeRepository) LikeIgnoreDuplicate(ctx context.Context, like *Like) (created bool, err error) {
	if like == nil || like.VideoID == 0 || like.AccountID == 0 {
		return false, nil
	}
	err = lr.db.WithContext(ctx).Create(like).Error
	if err == nil {
		return true, nil
	}
	if isDupKey(err) {
		return false, nil
	}
	return false, err
}

// ---------- 写：取消点赞 ----------

// DeleteByVideoAndAccount 非事务版：删流水，返回"这次真的删到行了吗"。同样是给阶段9 备的
// （deleted == false → 这条取消事件重复投递了，计数不能再减）。
func (lr *LikeRepository) DeleteByVideoAndAccount(ctx context.Context, videoID, accountID uint) (deleted bool, err error) {
	return lr.DeleteByVideoAndAccountTx(lr.db.WithContext(ctx), videoID, accountID)
}

// DeleteByVideoAndAccountTx 事务版。这里的 RowsAffected 是 unlike 方向的幂等凭据：
//
//	DELETE 删 0 行**不是错误**（GORM 什么都不报），所以不查它就会出现
//	"没点过赞的人点一下取消，计数凭空少 1"。
//
// 容易和 GREATEST 搞混的一点：`GREATEST(x-1, 0)` 只防**负数**，防不了**凭空变少**。
// 两个机制管的不是一回事，缺一个都不行。
func (lr *LikeRepository) DeleteByVideoAndAccountTx(tx *gorm.DB, videoID, accountID uint) (deleted bool, err error) {
	if videoID == 0 || accountID == 0 {
		return false, nil
	}
	res := tx.Where("video_id = ? AND account_id = ?", videoID, accountID).Delete(&Like{})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// Unlike 按 *Like 里的两个 ID 删（签名对齐原项目的四件套）。
// 本阶段无调用者：unlike 的删除必须在事务里做，走 DeleteByVideoAndAccountTx。
// 它转发给非事务版，顺带把两个 SQL 合成一份，也顺手挡住了 like 为 nil 的解引用
// （service 已经校验过，但 repo 直接 panic 比静默无操作更难查）。
func (lr *LikeRepository) Unlike(ctx context.Context, like *Like) error {
	if like == nil {
		return nil
	}
	_, err := lr.DeleteByVideoAndAccount(ctx, like.VideoID, like.AccountID)
	return err
}

// ---------- 读 ----------

// IsLiked 单条视角：这个人赞过这个视频吗。
//
// 用 Count 而不是 First：查无记录时 Count 返回 0 且**不报错**，正是我们要的
// "答案是 false"，而不是一个 gorm.ErrRecordNotFound 要上层去翻译。
//
// 注意它对**不存在的视频**同样返回 false（原项目的产品行为，不是漏判）：
// 查询接口幂等地回答"你赞过吗"，答案是"没有"，没必要为查不到的视频单独报错。
func (lr *LikeRepository) IsLiked(ctx context.Context, videoID, accountID uint) (bool, error) {
	var count int64
	if err := lr.db.WithContext(ctx).Model(&Like{}).
		Where("video_id = ? AND account_id = ?", videoID, accountID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// BatchGetLiked 批量视角：一次 IN 查询回答"这一页里哪些是我赞过的"。
//
// 这是阶段3 留的那个口子的堵法：Feed 每页几十条，逐条调 IsLiked 就是 N+1。
// 它和 IsLiked 的**语义必须严格一致**（一个给 bool、一个给集合），否则会出现
// "列表说没点、点进去说点了"。
//
// 两个短路：匿名（accountID == 0）和空 id 列表直接返回空 map。
// 返回的是 make 出来的**非 nil** map：调用方 `likedMap[video.ID]` 直接取，
// 取不到自然是 false，不用判存在、也不怕返回 nil。
func (lr *LikeRepository) BatchGetLiked(ctx context.Context, videoIDs []uint, accountID uint) (map[uint]bool, error) {
	liked := make(map[uint]bool)
	if accountID == 0 || len(videoIDs) == 0 {
		return liked, nil
	}
	var likes []Like
	if err := lr.db.WithContext(ctx).Model(&Like{}).
		Where("video_id IN ? AND account_id = ?", videoIDs, accountID).
		Find(&likes).Error; err != nil {
		return nil, err
	}
	for _, like := range likes {
		liked[like.VideoID] = true
	}
	return liked, nil
}

// ListLikedVideos 我的点赞列表：likes JOIN videos，按**点赞时间**倒序。
//
// 排序键是 likes.created_at 而不是 videos.create_time —— 用户想看的是"我最近赞了什么"，
// 不是"我最近赞的视频是什么时候发布的"。
//
// Limit(200) 是这个无游标接口的兜底：不设上限，一个赞了几千条的用户一次请求
// 就能把整表 JOIN 拉出来。
//
// .Select("videos.*") 不是装饰：JOIN 上的 SELECT * 会把 likes 的列一起吐出来，
// 而 likes 有 id / created_at / video_id，和 videos 的同名列**撞名** —— 靠 MySQL
// 的列顺序碰巧能扫对（feed/repo.go 的 ListByTag 是同一个形状，也确实能跑），
// 但那是在赌顺序。显式写 videos.* 把它钉死。
//
// 空结果返回 nil 切片（不是空切片），由 handler 负责转成 []Video{}：
// JSON 里 nil 序列化成 null，前端 v-for 会炸 —— 和 feed/handler.go 的
// nonNilFeedVideoItems 是同一个道理。
func (lr *LikeRepository) ListLikedVideos(ctx context.Context, accountID uint) ([]Video, error) {
	var videos []Video
	if accountID == 0 {
		return videos, nil
	}
	if err := lr.db.WithContext(ctx).Model(&Video{}).Table("videos").
		Select("videos.*").
		Joins("JOIN likes ON likes.video_id = videos.id").
		Where("likes.account_id = ?", accountID).
		Order("likes.created_at DESC").
		Limit(200).
		Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}

// ---------- 驱动细节 ----------

// isDupKey 判断 err 是不是 MySQL 的唯一键冲突（错误码 1062）。
//
// 必须用 errors.As 取出驱动的 *mysql.MySQLError 再比**错误码**，
// **绝不能拿 err.Error() 去匹配字符串** "Duplicate entry"：
// 那是 MySQL 的文案，换个版本、换个语言环境就变；错误码才是协议的一部分。
//
// （文档把它放在 like_service.go:26-29。它纯粹是驱动细节，跟着 repo 走更顺，
// 而且 LikeIgnoreDuplicate 也要用它；同包调用，放哪边功能上没差别。）
func isDupKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
