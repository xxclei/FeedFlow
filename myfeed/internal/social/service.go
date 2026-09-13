package social

import (
	"context"
	"errors"

	"myfeed/internal/account"
)

// SocialService 关注业务。
//
// 写路径的顺序是**刻意的**，而且和我们前面几个模块都不一样：
//
//	校验 → 直写 DB → [阶段7] 失效缓存 → [阶段9] 发 MQ（失败只记日志）
//
// 对比一下点赞/评论的最终形态（阶段9）：
//
//	范式 A（like / comment）：MQ 是**唯一**写路径，直写只是降级
//	    → 削峰彻底，但"读己之写"有延迟（刚点赞，刷新可能看不到）
//	范式 B（social，本模块）：直写 + MQ 冗余双写
//	    → 强一致、没有"关注了却看不到"的窗口，但 MQ 没减负（只承担通知/统计）
//
// 原项目两种范式混用是历史遗留，文档让你选一种贯彻。本模块按范式 B 实现，
// 因为**关注关系的写入频次天然低**（人一天关注不了几个），削峰没有收益，
// 而"关注完刷新就消失"对用户的伤害是实打实的。点赞是热点写，才值得付那个延迟。
//
// 顺序里还有一条通用纪律：**把"必须成功的"放前面、"锦上添花的"放后面**。
// DB 写失败 → 整个操作失败（对）；缓存失效失败 → 只记日志（对，缓存会自然过期）；
// MQ 发布失败 → 只记日志（对，通知丢了比关注失败轻得多）。
type SocialService struct {
	repo        *SocialRepository
	accountrepo *account.AccountRepository

	// 阶段7回填：cache *rediscache.Client
	// 阶段9回填：socialMQ *rabbitmq.SocialMQ
	//
	// 本阶段两个都没有 —— 不是"传 nil"，是类型还不存在，字段写不出来。
	// 和阶段4 的 NewLikeService 同一个处理（见那里的说明）：
	// 包建好了再把字段和参数补上，构造函数签名改一次。
}

func NewSocialService(repo *SocialRepository, accountrepo *account.AccountRepository) *SocialService {
	return &SocialService{repo: repo, accountrepo: accountrepo}
}

// Follow 关注。四层校验，每一层挡一种非法输入，**顺序不能乱**：
//
//	① 两端账号都存在（FindByID ×2）—— 没有外键，完整性自己兜
//	② 不能关注自己
//	③ IsFollowed 预检 —— 给用户友好文案（**只是体验**）
//	④ Insert —— 唯一索引兜底（**这才是防线**）
//
// ③ 和 ④ 的关系和阶段4 点赞一模一样：预检和插入之间永远有时间窗，
// 并发双击时两个请求都会通过 ③，然后由 ④ 拦下第二次。
// 区别是阶段4 把 1062 翻译成了业务错误，这里**没有翻译** —— 并发重复关注会
// 裸着变成 500（原项目如此，文档列为可改进项）。所以别指望 ③ 是可靠的：
// 它的价值是"99% 的重复点击得到一句人话"，不是数据正确性。
//
// ② 为什么必须排在 ③ 之前（文档这里说得不清楚，展开讲）：
// 把自关注校验放到 ③ 后面，**自关注会直接插入成功** —— 因为
// IsFollowed(我, 我) 本来就是 false（我没有关注我自己），预检放行，
// 然后 ④ 把这条自关注边写进表里。于是你多了一个粉丝：你自己。
// 顺带省一次 COUNT：关注自己的请求在 ② 就被挡下，不走 ③。
//
// （文档说"自关注会先被 already followed 误拦"只在**已经存在自关注脏数据**时成立，
//  描述的是症状不是机理。真正的理由是上面那句"会插入成功"。）
func (s *SocialService) Follow(ctx context.Context, social *Social) error {
	// ① 两端存在性。follower 来自 JWT，正常必然存在；vlogger 是客户端传的，必须查。
	//    两次 FindByID 不合并成一次 IN 查询：错误消息要能分清是哪一端不存在
	//    （虽然原项目两种情况返回的都是 gorm.ErrRecordNotFound 这个同一个错误）。
	if _, err := s.accountrepo.FindByID(ctx, social.FollowerID); err != nil {
		return err
	}
	if _, err := s.accountrepo.FindByID(ctx, social.VloggerID); err != nil {
		return err
	}

	// ② 自关注
	if social.FollowerID == social.VloggerID {
		return errors.New("can not follow self")
	}

	// ③ 预检
	isFollowed, err := s.repo.IsFollowed(ctx, social)
	if err != nil {
		return err
	}
	if isFollowed {
		return errors.New("already followed")
	}

	// ④ 唯一索引兜底（1062 不翻译，直接是 500）
	if err := s.repo.Follow(ctx, social); err != nil {
		return err
	}

	// 阶段7插这里：s.invalidateFollowingFeedCache(ctx, social.FollowerID)
	//
	// 届时做的事：按 pattern `feed:listByFollowing:*:accountID=<followerID>:*` 删掉
	// 这个用户的**所有游标页**缓存（SCAN + DEL，不是只删第一页 —— 只删第一页的话，
	// 翻到第二页还是会看到关注前的结果）。
	//
	// 为什么在这里还写不出占位方法：`*rediscache.Client` 这个类型还不存在，
	// 写 `if s.cache == nil { return }` 都编译不过（字段本身定义不出来）。
	// 一个"被调用但什么都不做"的空方法比一行注释更糟 —— 它会让人以为功能已就位。

	// 阶段9插这里：发 socialMQ.Follow(followerID, vloggerID)，失败仅 log.Printf
	//
	// 注意范式 B 的代价：MQ 消息和这次 DB 写入是**两条独立的写**，
	// Worker 消费时会再 Insert 一次同一条关系 → 撞唯一索引 1062 → Worker 忽略它。
	// 也就是说 MQ 那一路是"冗余的"，它存在只是为了触发通知（"关注了你"）。

	return nil
}

// Unfollow 取关。
//
// 语义上有一处**刻意的区分**，是文档里明确要保留的：
//
//	接口层：没关注过就取关 → 报错 "not followed"
//	异步层（阶段9 的 SocialWorker）：同样的删除 → RowsAffected == 0 静默通过
//
// 为什么同一个业务在两个入口语义不同：**接口层的错误是给用户看的**
// （"你的操作没生效"，需要让他知道），**异步层的幂等是给系统看的**
// （MQ 重复投递是正常的，重试不该报错）。把两者统一成一种会有一边难受。
//
// 注意这里的预检和 repo.Unfollow 之间也有时间窗，但**这次不需要防**：
// 并发取关第二次删 0 行、无副作用、返回 200 —— 结果和第一次一样是"没关注"，
// 自洽。这正是"看 RowsAffected 不是 DELETE 的通用纪律"那句话的用例。
func (s *SocialService) Unfollow(ctx context.Context, social *Social) error {
	if _, err := s.accountrepo.FindByID(ctx, social.FollowerID); err != nil {
		return err
	}
	if _, err := s.accountrepo.FindByID(ctx, social.VloggerID); err != nil {
		return err
	}

	// 注意 Unfollow **不查自关注**：取关自己本来就是"没关注过"，
	// 会走到下面的 "not followed"。原项目如此，语义上也说得通。
	isFollowed, err := s.repo.IsFollowed(ctx, social)
	if err != nil {
		return err
	}
	if !isFollowed {
		return errors.New("not followed")
	}

	if err := s.repo.Unfollow(ctx, social); err != nil {
		return err
	}

	// 阶段7插这里：s.invalidateFollowingFeedCache(ctx, social.FollowerID)
	// 阶段9插这里：发 socialMQ.UnFollow(...)，失败仅 log.Printf

	return nil
}

// GetAllFollowers 粉丝列表。先验目标账号存在（否则返回空列表会让人以为"这人有 0 个粉丝"，
// 而不是"这个人不存在"）。
func (s *SocialService) GetAllFollowers(ctx context.Context, vloggerID uint) ([]*account.Account, error) {
	if _, err := s.accountrepo.FindByID(ctx, vloggerID); err != nil {
		return nil, err
	}
	return s.repo.GetAllFollowers(ctx, vloggerID)
}

func (s *SocialService) GetAllVloggers(ctx context.Context, followerID uint) ([]*account.Account, error) {
	if _, err := s.accountrepo.FindByID(ctx, followerID); err != nil {
		return nil, err
	}
	return s.repo.GetAllVloggers(ctx, followerID)
}

// CountFollowers / CountVloggers 直通 repo，**不验账号存在性** ——
// 查一个不存在的 ID 自然得 0，而"0 个粉丝"就是正确答案，没必要为它查一次账号。
// 这是"计数接口"和"列表接口"的分界：列表要验（返回的内容需要有主），计数不用。
func (s *SocialService) CountFollowers(ctx context.Context, vloggerID uint) (int64, error) {
	return s.repo.CountFollowers(ctx, vloggerID)
}

func (s *SocialService) CountVloggers(ctx context.Context, followerID uint) (int64, error) {
	return s.repo.CountVloggers(ctx, followerID)
}

// 原项目还有一个 service.IsFollowed（两个端点存在性 + repo.IsFollowed），
// 是**死代码** —— 五个路由没有一个是"我关注他了吗"，handler 也从没调过它。
// 这里不抄。
//
// 但这暴露了一个真实的接口缺口：**前端没有任何办法问"我关注这个人了吗"**。
// 关注按钮因此在刷新后不知道该显示"关注"还是"已关注"（只能靠本地记忆）。
// 想补的话，最小改动是加一个 `POST /social/isFollowed {vlogger_id}`，
// 复用 repo.IsFollowed —— service 和 handler 各三行。见前端 AuthorCard 的说明。
