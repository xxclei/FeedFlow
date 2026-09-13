package video

import (
	"context"
	"errors"
	"log"
	"regexp"
	"strings"

	"myfeed/internal/account"
	"myfeed/internal/apierror"
	"myfeed/internal/notification"

	"gorm.io/gorm"
)

// CommentService 评论业务：发表 / 删除 / 列出，外加一个副作用（@提及通知）。
//
// 本阶段（阶段5）只有一条路径：**直写事务**。阶段9 会在这条路径前面插一层
// "先试 MQ 投递，失败才落到这里"，那时的顺序是：
//
//	mysqlEnqueued  := commentMQ.Publish(...)      // 评论落库交给 Worker
//	redisEnqueued  := popularityMQ.Update(+1)     // 热度交给 Worker
//	两个都成功 → notifyMentions 后直接 return（下面的直写整段跳过）
//	否则       → 落到下面的直写事务（就是现在这段代码，一行都不用删）
//
// 所以下面的直写**不是临时代码，它是永久的降级路径**。阶段5 不写 MQ 分支
// （写了也是死代码），等阶段9 再插。
type CommentService struct {
	repo      *CommentRepository
	videoRepo *VideoRepository

	// accountRepo 是 notifyMentions 用的：评论内容里的 "@bob" 要翻译成 AccountID。
	//
	// 原项目这一步是 `s.repo.db.Table("accounts").Where("username = ?")` 直接
	// 硬写表名查的（绕过了一切封装）。这里用 account 包自己的 repo ——
	// video 包依赖 account 包不是新引入的方向：video_handler.go 早就在依赖
	// account.AccountService 了（router.go 里那句"accountService 死依赖"）。
	accountRepo *account.AccountRepository

	// notificationRepo 是"通知表怎么写"的唯一入口。见 notification/repo.go
	// 上关于原项目裸 Table("notifications") 的说明。
	notificationRepo *notification.NotificationRepository
}

// NewCommentService 阶段5 四个依赖。
//
// 文档给的签名是 5 个参数（repo, videoRepo, cache, commentMQ, popularityMQ，
// 后三个本阶段传 nil），但 rediscache / rabbitmq 两个包现在还不存在、类型都没有，
// 写不出来。和阶段4 的 NewLikeService 同一个处理：建包时再把参数补上。
//
// 多出来的两个（accountRepo / notificationRepo）是文档版没有的 —— 因为原项目
// 用裸 db 查询代替了它们。那两个包建好之后，这个签名会变成 7 个参数，
// 那是阶段7/9 的事。
func NewCommentService(
	repo *CommentRepository,
	videoRepo *VideoRepository,
	accountRepo *account.AccountRepository,
	notificationRepo *notification.NotificationRepository,
) *CommentService {
	return &CommentService{
		repo:             repo,
		videoRepo:        videoRepo,
		accountRepo:      accountRepo,
		notificationRepo: notificationRepo,
	}
}

// Publish 发表评论。顺序：校验 → 视频存在 → 事务（插评论 + 热度+1）→ 提及通知。
//
// 前两步和阶段4 的 Like 同构：**预检只是体验，事务内的复查才是防线**。
// 从"查到视频存在"到"事务里插入"之间永远有时间窗，视频可能正好被作者删掉。
// 我们的表没有外键（GORM 不会自己加），"comments.video_id 一定指向存在的 videos"
// 这件事没有任何人替我们保证 —— 所以要在事务里手工再捡一次。
func (s *CommentService) Publish(ctx context.Context, comment *Comment) error {
	if comment == nil {
		return errors.New("comment is nil")
	}

	// TrimSpace 在这里做，而不是 handler —— 它是**业务规则**（"全是空格的评论不算评论"），
	// 不是参数形状问题。所有调用方（现在的 HTTP handler、阶段9 的 MQ Worker）
	// 都能因此受益，而不是各写一遍。
	//
	// 但这里留了一个原项目的瑕疵：handler 只查 `req.Content == ""`，
	// 所以"全是空格"会**穿透**到这里才被拦下，报的是裸 errors.New → **HTTP 500**。
	// 400 才对（这是参数问题）。修法有两种，都只改一行：
	//	① handler 改成 if strings.TrimSpace(req.Content) == "" → 400
	//	② 这里的错误换成 apierror.ErrValidation → 400
	// 本阶段按原项目保留 500（验收清单里明确写了"注意状态码 500"）。
	comment.Username = strings.TrimSpace(comment.Username)
	comment.Content = strings.TrimSpace(comment.Content)

	if comment.VideoID == 0 || comment.AuthorID == 0 {
		return errors.New("video_id and author_id are required")
	}
	if comment.Content == "" {
		return errors.New("content is required")
	}

	// 事务外预检：省一次开事务，且能给"视频不存在"一个友好错误
	exists, err := s.videoRepo.IsExist(ctx, comment.VideoID)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("video not found")
	}

	// 事务：插评论 + 热度 +1，同生共死。
	//
	// 阶段4 的 tx 逃逸教训在这里直接兑现：闭包里三个调用
	// （IsExistTx / CreateCommentTx / ChangePopularityTx）**全部显式吃 tx**。
	// 少一个 Tx 后缀，那条语句就会跑到事务外的那条连接上立刻提交 ——
	// 事务回滚了它也不回滚，留下"评论没插进去、热度已经 +1"的鬼数据。
	// 这个 repo 干脆不提供非 Tx 的写方法，就是为了让这种错**写不出来**。
	if err := s.repo.Transaction(ctx, func(tx *gorm.DB) error {
		exist, err := s.videoRepo.IsExistTx(tx, comment.VideoID)
		if err != nil {
			return err
		}
		if !exist {
			return errors.New("video not found")
		}

		// CreatedAt 交给 gorm:"autoCreateTime" 填（不像 Like 那样显式赋值）。
		// 差别在于：阶段9 的 LikeWorker 要能覆盖"事件发生时刻"，
		// 而评论的 publish 事件带的是完整内容、Worker 会新建一条评论行，
		// 那时再决定时间戳从哪来（原项目也是自动填）。
		if err := s.repo.CreateCommentTx(tx, comment); err != nil {
			// 这里**没有** isDupKey 分支 —— 不是漏了，是没有唯一索引可撞。
			// 评论允许一人对同一视频发多条，重复提交就是两条评论，产品语义如此。
			// 缺少 1062 这道防线，是评论模块比点赞弱一档的地方（阶段9 讨论
			// MQ 重复消费时会回到这个问题：事件里有 EventID，但没有去重表）。
			return err
		}
		return s.videoRepo.ChangePopularityTx(tx, comment.VideoID, +1)
	}); err != nil {
		return err
	}

	// 副作用放在**事务提交之后**，这是有顺序要求的：
	// 放进上面的事务里，通知就和评论绑成命运共同体了 ——
	// 通知表写失败会让一条本来能发表的评论回滚，说不通。
	s.notifyMentions(ctx, comment)
	return nil
}

// Delete 删评论。**属主校验在这一层，而且必须在任何删除动作之前**。
//
// 这一条不能下放给异步方，理由说清楚：阶段9 的 delete 事件里**只有 commentID**，
// Worker 无从知道"是谁在删"—— 它既没有请求上下文，也没有义务做安全判断。
// 安全校验只能在有身份的地方做，而有身份的地方就是这里。
//
// 校验顺序也是刻意的：先 GetByID 拿评论（顺带确认它存在），再比 AuthorID。
// 反过来先比权限就没有对象可比。
func (s *CommentService) Delete(ctx context.Context, commentID uint, accountID uint) error {
	comment, err := s.repo.GetByID(ctx, commentID)
	if err != nil {
		return err
	}
	// GetByID 的约定是 ErrRecordNotFound → (nil, nil)，所以这里必须判 nil
	if comment == nil {
		return errors.New("comment not found")
	}

	// 一行定权限。**403 不是 401**（本项目的改进，原项目用 401）：
	// 用户是合法登录的，只是这件事不归他做。前端拿到 401 会清 token 把人登出，
	// 权限错误混进 401 会让"点了别人评论的删除"变成一个登出事件。
	//
	// 注意比对的是 AuthorID，不是 Username —— username 是展示字段，
	// 可改名、可重名，拿它鉴权等于用名字当身份证。
	if comment.AuthorID != accountID {
		return apierror.ErrForbidden
	}

	return s.repo.Transaction(ctx, func(tx *gorm.DB) error {
		// 删评论。**必须看 RowsAffected**：DELETE 影响 0 行不是错误，err 是 nil。
		// 两个并发删除请求各自 GetByID 成功、各自删、各自给热度 -1，
		// 热度就被扣了两次 —— 而这正是原项目漏掉的那道检查。
		deleted, err := s.repo.DeleteCommentTx(tx, comment)
		if err != nil {
			return err
		}
		if !deleted {
			// 走到这里 = 同一个评论被并发删了两次（另一个请求先提交了）。
			// 报错回滚，热度不能再减。
			return errors.New("comment not found")
		}

		// 热度 -1：**原项目缺的一步，这里补上**（文档 Q5 把它记为已知瑕疵）。
		//
		// 为什么原项目补不了：阶段9 的 delete 事件里只带了 commentID，
		// Worker 还得先 GetByID 查回评论才知道 videoID，而它没做这一步 ——
		// 教训是"**事件里要带够上下文**"。我们这里是同步的、评论对象就在手上，
		// 所以 videoID 是免费的。但这也意味着：阶段9 改造后，事件里必须带上 videoID，
		// 否则这个 -1 在 MQ 路径下会丢。
		//
		// 不查"视频还在不在"：视频已被删的话，这条 UPDATE 命中 0 行、
		// 不报错也不改任何东西，结果自洽 —— 和 Unlike 不做存在性检查同一个理由。
		//
		// GREATEST(x-1, 0) 只防负数，防不了"凭空变少"，所以上面那道
		// RowsAffected 检查才是真正管事的那个。
		return s.videoRepo.ChangePopularityTx(tx, comment.VideoID, -1)
	})
}

// GetAll 某视频的全部评论。先确认视频存在 —— 原项目对不存在的视频报
// "video not found"（500），而不是像 IsLiked 那样幂等地返回空列表。
//
// 两个只读接口的取舍不一致，是原项目的历史选择（IsLiked 是后加的查询接口，
// 语义更"查询"；GetAll 沿用了早期的写法）。照抄，但心里有数：
// 前端打开一个已被删除的视频详情页时，评论区报 500 而不是"暂无评论"。
func (s *CommentService) GetAll(ctx context.Context, videoID uint) ([]Comment, error) {
	exists, err := s.videoRepo.IsExist(ctx, videoID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("video not found")
	}
	return s.repo.GetAllComments(ctx, videoID)
}

// mentionRegex 抽出评论里的 @用户名。
//
// 原项目是 `@(\w+)`。Go 的 \w 是 ASCII 的 [0-9A-Za-z_]，所以**中文用户名抽不到**：
// 如果你的系统允许中文昵称，"@小王 拍得好"不会产生任何通知。
//
// 文档建议换成 `@([\p{Han}\w]+)`，但那不是严格改进 —— 它会把
// "@bob的想法" 整串抽成一个用户名 "bob的想法"（\p{Han} 包含"的"），
// 于是 bob 反而收不到通知了。两种写法各有各的漏，谁也不能通吃。
//
// 真正可靠的做法是要求 @ 后面用空格/标点断开（或做 @ 选择器，像 B 站那样）。
// 本阶段保持原项目的 `@(\w+)`：验收用的是英文用户名，行为可预期。
var mentionRegex = regexp.MustCompile(`@(\w+)`)

// notifyMentions 给评论里被 @ 的人写通知。
//
// 这是全项目**第一个"不能失败的主流程的副作用"**。三个设计点：
//
//	① 失败只 log，不向上返回 error。通知发不出去不该让评论发表失败。
//	   这是阶段9 那套 MQ 的思路原型 —— MQ 就是把"尽力而为的副作用"
//	   从 log.Printf 工业化成"可靠投递 + 重试 + 死信"。
//	② 去重 + 跳过自己。一条评论里 @bob 三次只发一条；@自己不通知自己。
//	③ 查不到的用户**静默跳过**（不是错误）：@一个不存在的名字是常见的手滑，
//	   不是异常。原项目也是这么做的。
//
// 调用点在 Publish 的事务**之后**（见那里的注释）。
// 原注释提醒的一个不一致仍然存在：阶段9 之后评论落库是异步的，
// 而提及通知是**同步**写的 —— 极端情况下评论最终没落库、通知已经发出去了。
// 原项目接受这个不一致；要修就得让通知也走 MQ。
func (s *CommentService) notifyMentions(ctx context.Context, comment *Comment) {
	matches := mentionRegex.FindAllStringSubmatch(comment.Content, -1)
	if len(matches) == 0 {
		return
	}

	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		username := m[1]

		// 去重：FindAllStringSubmatch 会把重复的 @ 全返回来
		if seen[username] {
			continue
		}
		seen[username] = true

		// 跳过自己：@自己不该给自己发通知
		if username == comment.Username {
			continue
		}

		// username → AccountID。查不到（ErrRecordNotFound）直接跳过这一条，
		// 继续处理下一个提及 —— 一个名字打错不该让其他的 @ 也收不到通知。
		acc, err := s.accountRepo.FindByUsername(ctx, username)
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				log.Printf("notifyMentions 查询账号失败: username=%s, err=%v", username, err)
			}
			continue
		}
		if acc == nil || acc.ID == 0 {
			continue
		}

		n := &notification.Notification{
			RecipientID: acc.ID,
			SenderID:    comment.AuthorID,
			Type:        notification.TypeMention,
			TargetID:    comment.VideoID, // 点通知要能跳回那条视频
			Content:     comment.Username + " 在评论中提到了你",
		}
		if err := s.notificationRepo.Create(ctx, n); err != nil {
			// 只记日志 —— 这是①②两条纪律的落点
			log.Printf("notifyMentions 写通知失败: recipient=%d, err=%v", acc.ID, err)
		}
	}
}
