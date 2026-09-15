package message

import (
	"context"

	"myfeed/internal/account"
)

// MessageService 私信业务。**整个文件只有两个方法，加起来十几行。**
//
// 它是一个"薄壳 service" —— 对比 SocialService（四层校验 + 缓存失效 + MQ 双写）
// 和 VideoService（事务 + outbox + 缓存 + 多表）就会明白：
// **service 的厚度 = 业务规则的条数，不是架构必需的一层。**
// 一个只有"存下来"和"读出来"的模块，service 就该是薄的；
// 硬要给它加一层"看起来像个 service"的东西，就是纯负债。
//
// 顺带一个结构上的观察：原项目**没有**这一层（handler 直接调 repo，
// 甚至 handler 摸的是 `service.repo`）。本项目拆出来是为了让
// "校验放哪"这个问题有答案 —— 见下面 Send 的说明。
type MessageService struct {
	repo        *MessageRepository
	accountrepo *account.AccountRepository
}

func NewMessageService(repo *MessageRepository, accountrepo *account.AccountRepository) *MessageService {
	return &MessageService{repo: repo, accountrepo: accountrepo}
}

// Send 发私信。**这里是本项目对原项目唯一的实质改进。**
//
// 原项目 /message/send 不校验 to_id 是否存在 —— 给 `to_id = 99999999`
// 发私信会返回 200 和一条带 id 的 Message，然后这条消息**永远没人能读到**
// （收件人不存在，其本人也永远不会来 list 它）。数据静静地烂在表里。
//
// 一行 FindByID 就能挡住。为什么值得单独说：**这是"没有外键"的直接后果。**
// 如果 messages.to_id 有 FOREIGN KEY，这条 INSERT 会被数据库自己拒绝。
// 项目为了让 AutoMigrate 简单、为了能灵活删数据，全库不用外键 ——
// 那么**引用完整性就从数据库的活变成了 service 的活**，
// 忘一次就有一个入口能写脏数据。这条纪律在 social 里出现过（两端 FindByID），
// 在 comment/like 里出现过（video 存在性），这里是第三次。
//
// 想验证外键缺位的代价，可以试试把 account 删掉再 list 会话 ——
// 消息还在，只是 from_id 指向一个不存在的账号，前端画不出来。
//
// ---------- 关于"给自己发私信" ----------
//
// **允许**，不做 social 那样的 "can not follow self" 拦截。理由是两者的
// 后果不对称：
//
//	自关注 → 你的粉丝数 +1，是**数据污染**（一个不反映现实的计数）
//	自发私信 → 就是一条 to_id == from_id 的消息，是**合法数据**
//
// 而且自发的消息在 List 里能正确显示：会话查询是
// `(from=me and to=me) or (from=me and to=me)`，两个分支命中同一批行，
// 是行过滤的重复不是 join 的笛卡尔积，**不会重复出现**。
// （真要说的话，这个 OR 稍微浪费：自聊时可以短路成一条查询。不值得为此加分支。）
func (s *MessageService) Send(ctx context.Context, msg *Message) error {
	// 收件人必须存在。发件人来自 JWT，正常必然存在，不用查
	// （和 social 里 follower 那一端同理，但那里原项目查了、这里不查 ——
	//  区别是 social 的 Follow 有"两端对称"的语义，私信没有）。
	if _, err := s.accountrepo.FindByID(ctx, msg.ToID); err != nil {
		return err
	}

	// 落库。GORM 会把自增 id 和 created_at 回填进 msg，
	// 所以调用方拿到的指针已经是"落库后的对象" —— handler 直接序列化它即可。
	// 这就是为什么 repo.Send 接收指针而不是值。
	return s.repo.Send(ctx, msg)
}

// List 拉双向会话。
//
// **刻意不校验 peer 是否存在** —— 和 SocialService.GetAllFollowers 的做法相反，
// 理由值得说清（这两处其实是同一个判断的两面）：
//
//	粉丝列表：target 不存在 → 返回 [] 是**错的答案**（"这人有 0 个粉丝"
//	          ≠ "这个人不存在"）→ 所以必须先验，返回 ErrRecordNotFound
//	私信列表：peer 不存在 → 返回 [] 是**对的答案**
//	          （"你们的会话是空的" 与 "此人不存在"，对调用方是同一件事：
//	           没有任何消息可显示）→ 所以不用验
//
// 更需要说明的是：**即使验了也区分不出来。** 会话为空有两个正常来源
// （peer 存在但没聊过 / peer 不存在），响应体都是 `{"messages":[]}`，
// 想区分就得引入一个新的响应字段或错误码 —— 而前端对这两种情况的处理
// 完全相同（显示空会话）。**加一次查询换一个没人会读的区别，不加。**
//
// 真正的安全边界不在这一层：这是**"我的视角"接口**，self 来自 JWT，
// 所以哪怕 peer 传成任意 ID，能读到的也只有"我和他"的消息，
// 不存在越权读到别人会话的路径（见 entity.go 的 ListMessagesRequest）。
func (s *MessageService) List(ctx context.Context, selfID, peerID uint) ([]Message, error) {
	return s.repo.List(ctx, selfID, peerID)
}
