package social

import (
	"context"
	"errors"
	"log"
	"time"

	"myfeed/internal/account"
	"myfeed/internal/middleware/rabbitmq"
	rediscache "myfeed/internal/middleware/redis"
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

	// cache 阶段7接进来的：关注/取关后失效**关注流**的响应缓存。
	//
	// 注意 social 自己**不缓存任何东西** —— 它只用 cache 去删别人的 key。
	// 这看着别扭，但依赖方向是对的：缓存的是 feed 的响应，失效逻辑天然属于
	// "谁改了数据谁负责通知"。另一种做法是让 feed 去轮询 social 的变化，那是倒过来。
	// 可以为 nil（启动降级），nil 时失效变成空操作。
	cache *rediscache.Client

	// socialMQ 阶段9 接进来的。**范式 B 的关键差别就在这里**：
	// 它不是写路径，只是"写完之后再喊一嗓子"。可以认为它是一段
	// 工业化的 log.Printf —— 发失败只记日志，绝不影响这次关注的结果。
	//
	// 可以为 nil（RabbitMQ 启动连不上），nil 时整个 MQ 副作用静默跳过。
	socialMQ *rabbitmq.SocialMQ
}

func NewSocialService(repo *SocialRepository, accountrepo *account.AccountRepository, cache *rediscache.Client, socialMQ *rabbitmq.SocialMQ) *SocialService {
	return &SocialService{repo: repo, accountrepo: accountrepo, cache: cache, socialMQ: socialMQ}
}

// invalidateFollowingFeedCache 删掉某个用户的**全部**关注流页缓存。
//
// ---------- 为什么是 DelByPattern 而不是 Del 一个 key ----------
//
// 关注流的缓存 key 里有三样东西：limit、accountID、before（游标）。
// 而 before 取遍了所有翻过的页，是**不可枚举**的 —— 你没法知道用户翻过哪几页。
// 所以只能按模式删：`feed:listByFollowing:*:accountID=<id>:*`
// （limit 和 before 都用 * 吃掉）。
//
// 这就是"缓存 key 设计决定失效策略"的实例：如果 key 里没有 before
// （比如把整条流当成一个 key 缓存），失效就简单了，但翻页会全错。
// 反过来，key 越精确，失效越只能靠模式匹配 —— 而 SCAN 是 O(keyspace) 的。
// 原项目也接受这个代价，因为关注/取关是**低频写**（人一天关注不了几个）。
// 如果这是点赞那种热点写，正确做法是维护一个 `user:<id>:following_pages` 的
// Set 索引，靠集合精确删 —— 用空间换掉 SCAN。
//
// ---------- 为什么忽略错误、用 Background ----------
//
// DB 已经写成功了。删缓存失败只会让用户最多 24 小时（followingCacheTTL）
// 看不到新关注的人的动态，而返回错误会让用户看到"关注失败"却其实关注成功了 ——
// 后者更糟。和 video 的 invalidateDetails 是同一条纪律：
// **写路径的缓存失效是尽力而为的，TTL 才是最终保底。**
//
// 超时给 200ms（比一般的 50ms 宽）：SCAN 要遍历 keyspace，
// 比单条 DEL/GET 慢，用 50ms 容易在高 key 数量下稳定失败。
// 注意这里不接请求的 ctx —— 客户端断开不该让失效半途而废。
func (s *SocialService) invalidateFollowingFeedCache(followerID uint) {
	if s.cache == nil {
		return
	}
	// Key() 会把 v1: 前缀加上；DelByPattern **不会**自动加前缀（它直接透传给 SCAN），
	// 所以模式必须自己用 Key() 拼 —— 否则模式匹配不到任何东西，
	// 而且不会有任何报错，失效会静默失效。这是最容易埋雷的一行
	pattern := s.cache.Key("feed:listByFollowing:*:accountID=%d:*", followerID)

	opCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := s.cache.DelByPattern(opCtx, pattern); err != nil {
		log.Printf("[social] 失效关注流缓存失败 follower=%d: %v", followerID, err)
	}
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
//
//	描述的是症状不是机理。真正的理由是上面那句"会插入成功"。）
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

	// 阶段7：DB 写成功后再失效缓存 —— 顺序不能反。
	// 先删缓存再写库的话，中间那个窗口里别人的读请求会把**旧数据**重新填回缓存，
	// 于是这次失效等于没做（经典 cache-aside 竞态）。
	// 先写库再删缓存，最坏也只是"极短时间内的读拿到旧值"，不会永久脏。
	s.invalidateFollowingFeedCache(social.FollowerID)

	// 阶段9：MQ 冗余双写（范式 B）。**失败只记日志**，理由见类型注释。
	//
	// 范式 B 的代价如实记下：MQ 消息和上面那次 DB 写入是**两条独立的写**，
	// SocialWorker 消费时会再 Insert 一次同一条关系 → 撞唯一索引 1062 → 忽略它。
	// 也就是说 MQ 那一路是"冗余的"，它存在只是为了触发通知（"关注了你"）。
	//
	// 换个角度看，这个冗余是有用的：它让"关注成功但通知链路挂了"和
	// "关注都没成功"变成两件独立的事。前者用户可以接受，后者不行 ——
	// 而范式 A 里这两件事是绑在一起的（消息发不出去就走降级，一荣俱荣）。
	s.publishSocialMQ(ctx, "Follow", social, s.socialMQ.Follow(ctx, social.FollowerID, social.VloggerID))
	return nil
}

// publishSocialMQ 统一处理"投递结果"：**失败只记日志，绝不向上返回**。
//
// 为什么把 err 当参数传进来、而不是在这里判断收发哪一路：
// 收发本身一行就够，但"失败只记日志"这条纪律必须**只写一遍**。
// 写成 if s.socialMQ != nil { ... } 的话，两处调用点各有一套判断，
// 以后有人在其中一处顺手加个 `return err`，范式 B 就悄悄变成了
// "关注会因通知投递失败而失败"——而那正是本模块开头明确否掉的语义。
//
// 另外注意 s.socialMQ 为 nil 时这一行也是安全的：SocialMQ 的方法
// 都是**指针接收者且自查 nil**（见 socialMQ.go 的 publish），
// 所以 `(*SocialMQ)(nil).Follow(...)` 返回的是一个 error 而不是 panic。
// 这正是那种"守卫写在被调方比写在调用方更可靠"的例子 ——
// 调用方忘了判空，代价只是一条日志，不是一个 panic。
func (s *SocialService) publishSocialMQ(ctx context.Context, action string, social *Social, err error) {
	if err != nil {
		log.Printf("[social] %s 投递失败（关注关系已落库, 仅通知丢失）follower=%d vlogger=%d: %v",
			action, social.FollowerID, social.VloggerID, err)
	}
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

	// 阶段7：同上，DB 写成功后失效
	s.invalidateFollowingFeedCache(social.FollowerID)

	// 阶段9：同上，冗余双写，失败只记日志。
	//
	// 取关也要发消息，原因值得说一句：**通知链路只绑正向动作**
	// （`social.follow` → notification.social；取关了还给人发通知是骚扰），
	// 但 `social.events` 队列绑的是 `social.*`，所以取关**会**进 SocialWorker ——
	// 它要负责删掉那条 social 记录（冗余双写的另一半）。
	// 同一条消息，两条队列看到的是不同的子集，靠绑定 key 在交换机层分流。
	s.publishSocialMQ(ctx, "Unfollow", social, s.socialMQ.Unfollow(ctx, social.FollowerID, social.VloggerID))
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
