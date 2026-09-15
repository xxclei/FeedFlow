package worker

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"myfeed/internal/account"
	"myfeed/internal/middleware/jwt"
	rediscache "myfeed/internal/middleware/redis"
	"myfeed/internal/notification"

	"github.com/gin-gonic/gin"
)

// SSE 相关的三个常量。
const (
	// sseClientBuffer 每个连接的发送缓冲。
	//
	// 20 条是"用户离线期间攒的通知先替他握着"的额度。**它是有意的小**：
	// 缓冲越大，一个卡住的客户端占的内存越多，而且它并没有让通知更可靠
	// （真正可靠的补投是"重连后调 /notification/list 读表"）。
	// 满了就丢，见 Push。
	sseClientBuffer = 20

	// sseKeepalive 保活间隔。
	//
	// **这一条不是优化，是必需**：SSE 是普通的 HTTP 长连接，
	// 中间的任何一层（nginx 的 proxy_read_timeout 默认 60s、
	// 云负载均衡、公司出口防火墙）都可能因为"这条连接 60 秒没数据"
	// 而把它掐掉。发一行注释（以 `:` 开头，客户端会忽略）就能持续续命。
	//
	// 30 秒这个值是"比常见的 60s 超时小一半"，留足了余量。
	// 注释行的内容是给人看的 —— 抓包时能看到它是保活帧而不是真数据。
	sseKeepalive = 30 * time.Second

	// sseListLimit 通知列表一次最多返回多少条。和 repository 里的
	// LIMIT 50 是同一个数，但**不是同一个东西**：这里是接口契约
	// （前端只能拿到 50 条），那里是查询的 LIMIT 子句。
	// 两处都要有，不能只留一处。
	sseListLimit = 50
)

// SSEHub 是**每用户多连接**的订阅注册表。
//
// ---------- 为什么是 map[uint][]chan 而不是 map[uint]chan ----------
//
// 一个人可以同时开着手机、电脑、三个浏览器标签页 —— 每个都是一个独立的
// SSE 连接，都要收到同一条通知。所以值必须是**切片**。
//
// 这一点很容易写错：写成 `map[uint]chan` 的话，用户在第二个标签页
// 打开通知中心时，第一条连接会被**顶掉**（新 channel 覆盖旧的），
// 表现是"打开新标签页，老标签页的红点就不动了"——很难联想到是这里。
//
// ---------- 为什么用 RWMutex 而不是 sync.Map ----------
//
// 读写比极度悬殊（推送远远多于连接/断开），正是 RWMutex 的主场：
// Push 拿读锁可以完全并发，只有加/减连接时才需要写锁。
// sync.Map 在"读多写少"上也好，但它的接口不允许"遍历 + 修改"
// 这类复合操作做得很自然，而 Unsubscribe 需要（要在切片里找）。
//
// ---------- 这个结构在多实例部署下会坏掉 ----------
//
// 连接注册表在**进程内存**里，所以如果 API 起了两个实例，
// 用户连在实例 A 上，而处理点赞通知的 NotificationWorker 在实例 B 上跑 ——
// B 的 Push 找不到这个用户的连接，通知就推不到。
//
// 本项目是单实例，不做。**要知道它是单实例假设**，生产做法是把
// Push 这一跳改成 Redis pub/sub 或专门的推送网关。这是"内存注册表"
// 这类设计的通用边界，和 outboxworker.go 顶上说的"拆进程的前提"是同一条。
type SSEHub struct {
	mu      sync.RWMutex
	clients map[uint][]chan *notification.Notification

	// repo 是通知表的入口。SSEHub 需要它是因为 list/markRead/unreadCount
	// 三个接口都是**读表**，不是读内存 —— 表才是真相源，内存注册表
	// 只负责"实时推"这一件事。
	repo *notification.NotificationRepository

	// accountRepo / cache 只为了构造 SSE 的鉴权中间件，转手就交给 jwt 包。
	accountRepo *account.AccountRepository
	cache       *rediscache.Client
}

func NewSSEHub(repo *notification.NotificationRepository, accountRepo *account.AccountRepository, cache *rediscache.Client) *SSEHub {
	return &SSEHub{
		clients:     make(map[uint][]chan *notification.Notification),
		repo:        repo,
		accountRepo: accountRepo,
		cache:       cache,
	}
}

// Push 把一个通知推给某个用户的**所有**在线连接。
//
// 关键是 **select + default 的非阻塞发送**：
//
//	select {
//	case ch <- n:   // 有空间，推成功
//	default:        // 缓冲满（客户端卡住/网络慢），**直接放弃这一条**
//	}
//
// 如果写成阻塞的 `ch <- n`，一个卡住的客户端会把**整条消费链路**堵死：
// NotificationWorker 的 process 卡在这里 → 那条消息不 Ack →
// 后面所有用户的通知全部排在这条消息后面。**一个人的网速慢，
// 全站的通知都停了** —— 这是"一个客户端拖垮整个系统"的经典形态。
//
// 放弃是安全的，因为**通知已经落库了**（save 里先 Create 再 Push）。
// 前端重连/刷新时走 /notification/list 一定补得回来。
// 这条纪律可以记成：**推送可以丢，数据不能丢；所以推送永远不该阻塞。**
//
// 持读锁遍历：整个过程只读 clients，不修改它，所以读锁足够 ——
// 多个用户的通知可以并发推送，互不阻塞。这也是把这个 map 做成
// 全局单例而不是 per-request 的原因：它本来就是跨请求共享的状态。
func (h *SSEHub) Push(userID uint, n *notification.Notification) {
	if h == nil {
		return
	}
	h.mu.RLock()
	chans := h.clients[userID]
	h.mu.RUnlock()

	if len(chans) == 0 {
		return // 用户不在线。**这是正常情况，不是错误**，不记日志（会刷屏）
	}

	for _, ch := range chans {
		select {
		case ch <- n:
		default:
			// 这个连接满了。丢掉，等它重连后从表里补。
			log.Printf("[SSEHub] 连接缓冲已满, 丢弃本条推送 user=%d", userID)
		}
	}
}

// Subscribe 注册一条连接，返回它的接收 channel（带缓冲，见 sseClientBuffer）。
//
// **返回缓冲 channel 是这里唯一重要的事**：无缓冲 channel 的发送
// 需要接收方正在等待，而 SSE 的接收方是一个可能正在写 socket 的
// gin goroutine —— 于是 Push 会经常性地阻塞在"接收方还没轮到"上。
// 有了缓冲，Push 和写 socket 彻底解耦。
func (h *SSEHub) Subscribe(userID uint) chan *notification.Notification {
	ch := make(chan *notification.Notification, sseClientBuffer)
	h.mu.Lock()
	h.clients[userID] = append(h.clients[userID], ch)
	h.mu.Unlock()
	return ch
}

// Unsubscribe 注销一条连接。**必须在 SSEHandler 里 defer 调用**。
//
// 不注销的后果是**内存泄漏**，而且是最隐蔽的那种慢性泄漏：
// 用户每刷新一次页面就多一条永远关不掉的 channel（没人读它，
// 缓冲写满后就一直是满的），Push 时还会遍历到它、往里面塞、
// 塞不进去再记一条日志。跑一天下来，一个活跃用户能攒出几百条死连接，
// 表现是"服务越跑越慢、日志里全是缓冲已满"。
//
// close(ch) 的两个前提，都要保证才能调：
//
//	① **必须先从 map 里摘掉，再 close**。反过来的话，中间那个窗口里
//	   Push 可能刚拿到这条 channel 的引用，然后 close 了 —— 往已关闭的
//	   channel 发送会 **panic**（send on closed channel）。
//	   先摘后关，Push 要么看到它（并且能安全发送），要么看不到它，两全。
//	② **调用方（SSEHandler）必须已经停止从它读**。这里 close 之后
//	   再有人读会拿到零值 —— 但 SSEHandler 是同一条 goroutine 里
//	   defer 这个函数的，所以读循环一定已经退出了。**换成两条 goroutine
//	   收发就出事了**，见 SSEHandler 里的说明。
func (h *SSEHub) Unsubscribe(userID uint, ch chan *notification.Notification) {
	h.mu.Lock()
	defer h.mu.Unlock()

	list := h.clients[userID]
	for i, c := range list {
		if c == ch {
			// 从切片里删掉第 i 个。**必须重新赋值 h.clients[userID]** ——
			// append 之后切片的底层数组可能已经换了，只改局部变量 list
			// 不会写回 map（这是 Go 切片最容易踩的一个坑）。
			h.clients[userID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	// 这个用户已经没有连接了 → 把 map 里的键也删掉。
	// 不删的话，每个访问过的用户都会在 map 里留下一个空切片，
	// 又是一个"看不见的"内存增长（比上面那个小，但同样是泄漏）。
	if len(h.clients[userID]) == 0 {
		delete(h.clients, userID)
	}

	close(ch)
}

// ---------- HTTP 接口 ----------

// SSERequireAuth 转发到 jwt 包的 QueryTokenAuth。
//
// 文档把它写成 hub 的方法，这里偏离成一行转发 —— 理由写在
// jwt.QueryTokenAuth 的注释里（结论：安全逻辑不能有第二份实现）。
// 保留这个方法名是为了让 router.go 的接线读起来和文档一致。
func (h *SSEHub) SSERequireAuth() gin.HandlerFunc {
	return jwt.QueryTokenAuth(h.accountRepo, h.cache)
}

// SSEHandler 是那条**永不返回**的长连接。
//
// 响应头三件套，少一个都会出问题：
//
//	Content-Type: text/event-stream   ← 不写的话浏览器不认这是 SSE，
//	                                     EventSource 会直接触发 onerror
//	Cache-Control: no-cache           ← 不写的话中间的代理可能缓存这条
//	                                     流（SSE 是"内容不断增长的响应"，
//	                                     缓存它就等于永远收不到新帧）
//	Connection: keep-alive            ← HTTP/1.1 的显式声明（其实默认就是，
//	                                     写出来是为了自文档）
//
// ---------- 为什么收发在同一个 goroutine 里 ----------
//
// 这个 for/select 同时干三件事：读 ctx（客户端断开）、读 ch（有通知）、
// 等 30 秒（发保活）。**没有任何一个动作会阻塞**，所以一条 goroutine 够。
//
// 常见的写法是"一个 goroutine 阻塞读 ch，另一个处理 ctx"，
// 那样反而更危险：读 ch 的那个在 ctx 取消后还挂在 `<-ch` 上，
// 而 Unsubscribe 会 close(ch) —— 它拿到零值继续往外写
// 已经关闭的 HTTP 连接。**能写成一条 goroutine 就写成一条。**
func (h *SSEHub) SSEHandler(c *gin.Context) {
	userID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	ch := h.Subscribe(userID)
	// defer 的顺序是有讲究的：**先 Unsubscribe 再打日志**，
	// 保证 close(ch) 一定发生在函数退出前（defer 是 LIFO，
	// 所以 Unsubscribe 写在后面会先执行 —— 这里就写一行，不用纠结）。
	defer h.Unsubscribe(userID, ch)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // nginx 专用：关掉它自己的响应缓冲，否则帧会被攒着不发

	// 先发一个**注释帧**再 Flush，而不是只 Flush 响应头。
	//
	// ---------- 为什么"只 Flush 头"不够（阶段12 实测出来的） ----------
	//
	// 直接用 curl 打后端时，`c.Writer.Flush()` 确实让响应头在 0.05s 就到了。
	// 但**经过 vite 的开发代理**（浏览器里的真实路径）时，响应头要等到
	// 30 秒后第一条 keepalive 才出来 —— 实测：
	//
	//	直连 127.0.0.1:8080   headers @0.07s
	//	经 vite（:5173/api）   headers @30.02s   ← 卡了整整一个 keepalive 周期
	//
	// 原因是 Node 的 `res.writeHead()` **只把状态行和头记在内存里，不落到 socket**，
	// 真正发出去是在第一次 `write()` 时（它要等到那一刻才知道该用
	// Content-Length 还是 chunked）。代理没收到上游的 body 字节，就没有那次 write。
	//
	// 后果是前端 `onopen` 要等 30 秒才触发 —— 面板上会一直挂着
	// "实时推送未连上"，虽然推送本身是通的（实测：4.02s 发出的关注通知
	// 在 4.03s 就到达了，因为它就是那"第一个字节"）。
	//
	// **注释帧（以 `:` 开头的行）是标准 SSE 的一部分**：浏览器会忽略它，
	// 不进 onmessage、不产生任何数据 —— 所以它只干一件事：
	// 给出那个"第一个字节"，把响应头和连接状态一次性顶出去。
	// 任何"攒到有 body 才转发"的中间层（Node 代理、部分 nginx 配置、CDN）
	// 都会因此立刻放行。**代价是两个换行符，收益是 onopen 从 30s 变成 0s。**
	if _, err := fmt.Fprint(c.Writer, ": connected\n\n"); err != nil {
		return // 写不出去说明对端已经断了，直接收摊（defer 会清理订阅）
	}
	c.Writer.Flush() // 把注释帧顶出去 —— 此刻响应头才真正上到 wire

	// c.Request.Context() 在这里是**正确**的 ctx —— 和 outbox 轮询器那条
	// "绝不能用请求 ctx"正好相反，因为这条连接的整个生命周期**就是**
	// 这个请求的生命周期。客户端断开 → ctx Done → 循环退出 → defer 清理。
	// **ctx 的对错没有通则，取决于任务和请求的生死关系。**
	ctx := c.Request.Context()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case n := <-ch:
			// SSE 的帧格式：`data: <内容>\n\n`。**末尾那个空行是帧分隔符**，
			// 漏了它浏览器会把后面所有帧当成同一条的两行。
			// 这里用 c.SSEvent 也可以，但它会多加一个 `event:` 行；
			// 前端只认 data，所以直接手写更可控。
			if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", mustJSON(n)); err != nil {
				return // 写到一半连接断了 → 直接退出，defer 会清理
			}
			// **Flush 是 SSE 的命门**：不 Flush 的话数据停在 gin/net-http
			// 的缓冲里，前端要等到缓冲满或连接关闭才收到 —— 那就完全
			// 不是"实时"了。每一帧之后都要 Flush。
			c.Writer.Flush()

		case <-keepalive.C:
			// 注释帧：以 `:` 开头，按规范客户端必须忽略。
			// 它唯一的作用是让这条连接**持续有字节流动**，
			// 免得被中间层按"空闲超时"掐掉（见 sseKeepalive 的说明）。
			if _, err := fmt.Fprint(c.Writer, ": keepalive\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		}
	}
}

// ListHandler 拉最近 50 条通知。**无请求体**。
//
// 空结果返回 `[]` 而不是 `null` —— 前端 `v-for` 遇到 null 会炸。
// 这和阶段3 feed、阶段5 评论的 `nonNilXxx` 兜底是同一条纪律：
// **JSON 的 null 和空数组对 Go 来说是两个值，对前端来说是两种崩溃方式。**
// 兜底放在 HTTP 层（不放在 repo），因为它描述的是"接口契约"而不是"数据形状"。
func (h *SSEHub) ListHandler(c *gin.Context) {
	userID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	list, err := h.repo.ListByRecipient(c.Request.Context(), userID, sseListLimit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if list == nil {
		list = []notification.Notification{}
	}
	c.JSON(http.StatusOK, gin.H{"notifications": list})
}

// MarkReadHandler 标记已读：带 id 标一条，不带 id **全标**。
//
// "不带 body" 和 "body 是 {}" 都必须走全标 —— 所以**不能**用
// ShouldBindJSON 的报错来判分支（空 body 会报 EOF，{} 不会，
// 而两者的业务语义是完全一样的）。用 `err == nil && req.ID > 0`
// 这个组合判断，两种输入自然都落到全标分支。
//
// 返回体是固定的 `{"message":"ok"}`，**不报告实际标了几条**：
// 前端不关心，而且报告行数会泄漏"这个 id 存不存在"的信息
// （对比 MarkRead 的 RowsAffected 在 repo 层被用来判 404 —— 那里是必要的，
// 这里没必要把它透出去）。
func (h *SSEHub) MarkReadHandler(c *gin.Context) {
	userID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	var req struct {
		ID uint `json:"id"`
	}
	// 绑定的错误**故意忽略** —— 空 body、{} 、{"id":5} 三种都要能工作，
	// 而只有第三种不报错。详见上面的说明。
	_ = c.ShouldBindJSON(&req)

	if req.ID > 0 {
		if _, err := h.repo.MarkRead(c.Request.Context(), req.ID, userID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else {
		if _, err := h.repo.MarkAllRead(c.Request.Context(), userID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"message": "ok"})
}

// UnreadCountHandler 未读数（前端那个红点上的数字）。
//
// 这个接口会被前端**轮询**（除了 SSE 之外还有一层兜底），
// 所以它必须便宜：一次 COUNT，走 idx_recipient。
// 如果它变慢了，整个页面的心跳都会跟着变慢。
func (h *SSEHub) UnreadCountHandler(c *gin.Context) {
	userID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	count, err := h.repo.CountUnread(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"count": count})
}

// RegisterRoutes 挂四个接口。**鉴权已经由调用方（router.go）在 group 上挂好了**，
// 所以这里不再挂一遍 —— 挂两遍不会报错（check 跑两次，Redis 查两次），
// 只会让"这个接口到底受什么保护"看不清楚。
//
// stream 是 **GET**，其余三个是 POST —— 不是风格混搭：
// EventSource 只能发 GET，而另外三个带 body/有副作用，用 POST 语义更准。
func (h *SSEHub) RegisterRoutes(group *gin.RouterGroup) {
	group.GET("/stream", h.SSEHandler)
	group.POST("/list", h.ListHandler)
	group.POST("/markRead", h.MarkReadHandler)
	group.POST("/unreadCount", h.UnreadCountHandler)
}

// mustJSON 把通知序列化成 JSON。
//
// 名字里的 must 有点名不副实 —— 它**不会 panic**：序列化一个结构体
// 失败在实践中只可能是"结构体里有 chan/func 这类无法序列化的字段"，
// 那是编译期就该发现的事。真失败了就返回一个空对象 `{}`，
// 让这一帧变成一条空通知（前端会忽略），而不是让整个 SSE 连接崩掉。
//
// **一条通知的序列化失败不该断掉一个用户的所有后续通知。**
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("[SSEHub] 通知序列化失败（本条丢弃, 连接保留）: %v", err)
		return "{}"
	}
	return string(b)
}
