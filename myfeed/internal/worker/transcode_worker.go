package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"myfeed/internal/config"
	"myfeed/internal/middleware/rabbitmq"
	rediscache "myfeed/internal/middleware/redis"
	"myfeed/internal/transcode"
	"myfeed/internal/video"

	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
)

// TranscodeWorker 消费 video.transcode.queue，把一条高码率源片转成多档 HLS。
//
// 它和本包里其余五个 worker 有**三处本质区别**，每一处都是"任务耗时以分钟计"
// 这一个事实推出来的。写在这里，因为这是唯一一个能一次说清它们的地方：
//
//	① Qos(1)         别的 worker 是 50。50 意味着"我手上可以同时有 50 条未 Ack"，
//	                  对毫秒级任务是对的（吞吐）；对分钟级任务是灾难 ——
//	                  50 条消息会指使 50 个 ffmpeg **同时**跑起来，
//	                  32 个核瞬间被打满，而它们全都在做同一件事的 50 份拷贝。
//	                  1 = 串行，占用可预测。
//
//	② 不做优雅等待    别的 worker 关进程时会等在途消息处理完（handleDelivery 的
//	                  ctx.Done() 分支 Nack 回队列）。转码**等不起**：
//	                  一条 10 分钟的视频意味着 Ctrl+C 后要等十分钟。
//	                  这里靠的是"不 Ack 就会被重投"—— 把优雅退出这件
//	                  需要人去守的纪律，换成 broker 保证的机制。
//	                  （落点：transcode.Run 用的是 exec.CommandContext，
//	                    ctx 一取消就杀掉 ffmpeg 子进程，不会留孤儿进程。）
//
//	③ 失败即终态      别的 worker 失败会重试 4 次（1s/2s/4s 退避）。转码失败
//	                  绝大多数是**确定性的**（源文件坏、编码不被支持），
//	                  重试 4 次 = 白烧 4 倍 CPU 换同一个失败。所以除了
//	                  "DB 连不上"这类瞬时错误，其余的写 `failed` 就 Ack 收工。
//	                  重投（几小时后）会再试一次，那才是真正的重试。
//
// ---------- 幂等：靠什么保证重复执行无害 ----------
//
// 消息是 at-least-once 的，所以 transcode() 必须能被安全地重入。三条：
//
//	产物在唯一目录里   每次运行一个 runID（见 config.HLSRelDir 那段），
//	                  两次运行不可能写同一批文件
//	master 是提交点    产物全部写完才重写 master，所以不存在
//	                  "master 指向半截产物"这个状态
//	状态可重算         hls_url / hls_dir 每次都从 videoID 重算，不用消息里带的
//
// 还有一条**顺带得到的性质**：两个 worker 同时处理同一条视频（有人把
// worker 实例数改成 2）也不会互相破坏 —— 各写各的目录，各写各的 master，
// 后写的赢，两个都指向完整的产物树。只是白转一遍。
//
// ---------- 它为什么在 worker 进程里 ----------
//
// 判据见 outboxworker.go 开头那段：**只碰 DB + 文件系统、和 API 进程零共享资源
// 的就该拆出去**。转码完全符合：它不推 SSE、不读 Redis、不影响任何接口的响应。
// 反过来，如果放在 API 进程，一个 10 分钟的 ffmpeg 会把 32 个核里的 31 个占住，
// 而 API 的全部价值就是低延迟。
type TranscodeWorker struct {
	ch     *amqp.Channel
	videos *video.VideoRepository
	// cache 可以为 nil（启动时 Redis 没连上）。**nil 是正常状态不是错误**：
	// 转码的全部功能不依赖 Redis，只是在改完 DB 之后没法顺手删掉那两份副本，
	// 于是播放端最多晚 5 分钟（detailCacheTTL）才看到 hls_url。
	// 详见 markTranscode 的说明。
	cache   *rediscache.Client
	storage config.StorageConfig
	tcfg    config.TranscodeConfig
	queue   string
}

func NewTranscodeWorker(
	ch *amqp.Channel,
	videos *video.VideoRepository,
	cache *rediscache.Client,
	storage config.StorageConfig,
	tcfg config.TranscodeConfig,
	queue string,
) *TranscodeWorker {
	return &TranscodeWorker{
		ch: ch, videos: videos, cache: cache,
		storage: storage, tcfg: tcfg, queue: queue,
	}
}

// markTranscode 是**唯一**写 transcode_status / hls_url 的出口：
// 先落库，再删掉那两份缓存副本。
//
// ---------- 为什么必须合成一个方法 ----------
//
// 阶段 C 第一版是三个调用点各自直接调 `w.videos.MarkTranscode(...)`，
// 于是**三处都漏了删缓存** —— 而漏了不会有任何报错：
//
//	DB 里 transcode_status 已经是 ready / hls_url 有值
//	Redis 里那份还是发布时的 pending / hls_url=''
//	getDetail 读的是 Redis（阶段7 缓存旁路）→ 前端拿到 pending
//	→ 走直传 mp4 那条路，**转码产物一次都不会被请求**
//
// 实测复现过：往 `v1:video:detail:id=383` 种一份 pending，
// DB 是 ready，getDetail 照样返回 pending —— 而这份脏值能活满 5 分钟。
// 触发窗口也不是边角情况，而是**最常见的那条路**：
// 发布 → 上传者立刻打开自己的详情页（此时还是 pending）→ 转码完成。
//
// 所以这里不是"加一行记得删缓存"，而是把写 DB 和删缓存**绑成一个动作**：
// 调用方只要想改状态就必然经过这里，没有第二个能写成功的入口。
// 这和阶段 C 布局上用 runID 那一层是同一个思路 —— 靠结构，不靠纪律。
func (w *TranscodeWorker) markTranscode(
	ctx context.Context, videoID uint, status, hlsURL, hlsDir string,
) error {
	if err := w.videos.MarkTranscode(ctx, videoID, status, hlsURL, hlsDir); err != nil {
		// 库都没写成功就没有"库里新、缓存旧"这回事，不用删
		return err
	}
	video.InvalidateVideoCaches(ctx, w.cache, videoID)
	return nil
}

// Run 开始消费，阻塞直到 ctx 取消或 channel 断开。
//
// ---------- Qos(1) 为什么写在这里，而不是在消费骨架里加个参数 ----------
//
// cmd/worker/main.go 的 runWorkerWithRetry 在建好 channel 之后统一设了
// `Qos(50)`，然后才调 fn(ch)。也就是说**这里是覆盖，不是设置**：
// 同一个 channel 上第二次 Qos 调用生效，broker 按最后一次算。
//
// 这不是我发明的写法 —— NotificationWorker 的 consumeNotifications 已经在做
// 同一件事（它设 10，理由写在那个函数里）。所以这里遵循的是既有约定：
// **默认值在骨架里，例外在例外自己的构造函数里，且必须写清为什么例外。**
//
// Qos 失败只记日志不中断：它和别的地方一样是个优化（无背压也能跑，
// 只是会一次收到很多条）。但**对转码这条链路，Qos 失败的实际后果比别处严重**
// —— 见上面 ①，50 个并发 ffmpeg 是能把开发机拖死的。所以这里记的日志
// 明确说了后果，而不是照抄别处那句"继续，无背压"。
func (w *TranscodeWorker) Run(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if err := w.ch.Qos(1, 0, false); err != nil {
		log.Printf("[TranscodeWorker] 设置 Qos(1) 失败（继续，但可能同时跑多个 ffmpeg 打满 CPU）: %v", err)
	}
	return runConsumer(ctx, w.ch, "TranscodeWorker", w.queue, w.process)
}

// process 反序列化 + 校验 + 分发。返回 error 表示"值得重试"。
//
// 和 LikeWorker 一样，decodeEvent 的 ok=false（坏 JSON）和"字段缺失"
// 都返回 nil —— 重试不会让缺失的字段长出来。
func (w *TranscodeWorker) process(ctx context.Context, body []byte) error {
	evt, ok := decodeEvent[rabbitmq.TranscodeEvent]("TranscodeWorker", body)
	if !ok {
		return nil
	}
	if evt.VideoID == 0 {
		return nil
	}
	return w.transcode(ctx, evt.VideoID)
}

// transcode 一条视频的完整转码流程。
//
// ---------- 顺序不是随便排的 ----------
//
//	① 读 DB        拿 PlayURL（要拿它反查源文件路径）
//	② 产物已在？   是 → 跳到 ⑦ 只补状态（重投/被杀之后的捷径，**不重跑 ffmpeg**）
//	③ 探源文件     拿真实宽高（不信 DB 里的 src_*，理由见下）
//	④ 算档位       够不着任何一档 → 终态失败
//	⑤ 写 running   在这一步之后进程死掉，状态停在 running —— 那正是"看得见"
//	⑥ 转码 + 校验 + 写 master（**提交点**）
//	⑦ 写 ready     失败会返回 error，但重试会走 ② 的捷径，不会重跑 ffmpeg
//
// ---------- ③ 为什么不直接读 DB 里的 src_* ----------
//
// 那两个列是发布时门禁探的，看起来正好是我们要的东西。但：
//
//   - **可能是 0**：存量视频（transcode_status 空串那 127 条）从没经过门禁，
//     src_* 全是 0。想给它们补转码就必须自己探。
//   - **可能过期**：源文件是可以被换掉的（重新上传到同一路径）。
//     按一个过期的尺寸去生成档位，会产出一棵**放大的**产物树
//     （把 720p 拉到 1080p），不报错，只是更糊更大。
//
// 探测是读文件头（~1ms），和它防住的东西比不值一提。
// 一句话：**能重算的都不要信存量，因为存量会过期，重算的不会。**
// （同 TranscodeEvent 为什么不带参数的注释。）
func (w *TranscodeWorker) transcode(ctx context.Context, videoID uint) error {
	started := time.Now()

	// ---------- ① 读 DB ----------
	v, err := w.videos.GetByID(ctx, videoID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 视频被作者删了，而转码任务是在它删除之前入队的。
			// 属于"迟到的合法消息"：业务上已经无效，不算处理失败。
			log.Printf("[TranscodeWorker] video=%d 已不存在（被删除），丢弃转码任务", videoID)
			return nil
		}
		// 其余都是瞬时错误（DB 不可用、连接被掐）。返回 error → 走重试。
		return err
	}

	masterPath := w.storage.HLSMasterDiskPath(videoID)
	hlsRoot := w.storage.HLSRoot(videoID)

	// ---------- ② 产物已经在磁盘上了吗 ----------
	//
	// 这个捷径覆盖三种真实场景，每一种都**不该重跑一遍 ffmpeg**：
	//
	//	a. 转码成功、master 写好了，但最后那步写 DB 失败 → 重投
	//	b. 转码成功之后进程被 Ctrl+C 杀掉（消息没 Ack）→ 重投
	//	c. 有人手工重投了一条早就 ready 的消息
	//
	// 判据取的是**产物**（master 里每档的 playlist 都非空）而不是 DB 状态：
	// 因为 DB 状态恰恰是可能写失败的那一半，而磁盘是已经成功的那一半。
	// 这个不对称是刻意的 —— 用可靠的那一侧去推断不可靠的那一侧。
	if tiers, err := transcode.VerifyMaster(masterPath); err == nil {
		w.sweep(hlsRoot, masterPath)
		return w.markReady(ctx, v.ID, tiers, started, "产物已在磁盘上，跳过转码")
	}

	// ---------- ③ 探源文件 ----------
	srcPath, ok := w.storage.DiskPath(v.PlayURL)
	if !ok {
		// play_url 不在 UploadRoot 内（穿越/格式不对）。这是**终态**：
		// 重投多少次都还是同一个字符串。
		return w.fail(ctx, videoID, "play_url 不在 UploadRoot 内: %q", v.PlayURL)
	}
	info, err := video.ProbeMP4(srcPath)
	if err != nil {
		// 两种可能：文件不在（上传/发布解耦，或者被手工清理过）、
		// 文件坏了。两种都是终态 —— 等一会儿重投它也不会变好。
		return w.fail(ctx, videoID, "源文件探测失败 %s: %v", srcPath, err)
	}

	// ---------- ④ 算档位 ----------
	tiers := video.UsableTiers(info.Width, info.Height)
	if len(tiers) == 0 {
		// 源比最低档还小。门禁不会把这种素材入队（decideTranscode 第 3 条），
		// 但这是一条**独立的防线**：worker 也可能被手工重投、或者被别的
		// 路径（将来的"重新转码"接口）调用，那时门禁不在链路上。
		return w.fail(ctx, videoID,
			"源 %dx%d 够不着任何档位（最低档短边 %d）", info.Width, info.Height, lowestHeight())
	}

	runID := transcode.NewRunID()
	runDir := w.storage.HLSRunDir(videoID, runID)
	jobs := transcode.Plan(srcPath, info.Width, info.Height, runDir, tiers)

	// ---------- ⑤ 写 running ----------
	//
	// 在**开跑之前**写。这个顺序的意义是：进程在这一步之后任何地方死掉，
	// 库里都会留下 `running` 这个痕迹。
	//
	// 注意它顺手清空了 hls_url —— 对一条重转的、原本 ready 的视频，
	// 这会让前端立刻落回直传原文件（能播，只是慢），而不是继续指向
	// 一个即将被重写的 master。宁可降级，不可指向半截产物。
	if err := w.markTranscode(ctx, videoID, video.TranscodeRunning, "", runDir); err != nil {
		return err // 瞬时：DB 写不进去。重试不会浪费 ffmpeg（还没开跑）
	}

	log.Printf("[TranscodeWorker] video=%d 开始转码 run=%s 源=%dx%d@%dkbps %dms → %d 档 %v（%s）",
		videoID, runID, info.Width, info.Height, info.BitrateKbps, info.DurationMS,
		len(jobs), tierNames(tiers), v.PlayURL)

	// ---------- ⑥ 转码 ----------
	for _, j := range jobs {
		if err := os.MkdirAll(j.Dir, 0o755); err != nil {
			return w.fail(ctx, videoID, "建目录失败 %s: %v", j.Dir, err)
		}
		if err := transcode.Run(ctx, w.tcfg.FFmpegPath, j.Args); err != nil {
			// ctx 被取消（Ctrl+C）时这里也会进来。**不能标 failed** ——
			// 那不是转码失败，是我们自己要走。返回 error 让消息不被 Ack，
			// 等下次启动重投。
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return w.fail(ctx, videoID, "%s 档转码失败: %v", j.Tier.Name, err)
		}
	}

	// ---------- ⑥' 提交 ----------
	//
	// 顺序：写 master（提交点）→ 校验 → 清旧 run → 写 DB。
	//
	// 校验放在写 master **之后**，是因为这个顺序下"校验通过"证明的
	// 恰好是我们要的那件事：**这份 master 指向的东西是可播的**。
	// 反过来先校验再写，校验证明的只是"目录里有东西"，和 master 无关。
	if err := transcode.WriteMaster(masterPath, transcode.MasterPlaylist(runID, jobs)); err != nil {
		return w.fail(ctx, videoID, "写 master 失败: %v", err)
	}
	n, err := transcode.VerifyMaster(masterPath)
	if err != nil {
		// 走到这里说明刚写出来的 master 有问题 —— 产物是残缺的。
		// 标 failed 而不是 ready：宁可让前端直传原文件，也不要让它
		// 去拉一棵残缺的 HLS 树（那会让"播放失败"这个现象出现在用户那边，
		// 而不是出现在日志里）。
		return w.fail(ctx, videoID, "master 校验失败: %v", err)
	}
	w.sweep(hlsRoot, masterPath)

	// ---------- ⑦ 写 ready ----------
	//
	// 这里返回的 error 会触发重投，而重投会走 ② 的捷径（产物已完整）——
	// 代价是一次文件读，不是一次重转。这正是把"提交点"放在 DB 之前的意义：
	// **让最后一步失败变得便宜**，否则"写 DB 失败"会白白烧掉一次分钟级转码。
	return w.markReady(ctx, v.ID, n, started, fmt.Sprintf("run=%s", runID))
}

// markReady 写 ready 状态并汇报占用。
func (w *TranscodeWorker) markReady(ctx context.Context, videoID uint, tiers int, started time.Time, why string) error {
	hlsURL := config.HLSMasterURL(videoID)
	hlsDir := w.storage.HLSRoot(videoID)

	if err := w.markTranscode(ctx, videoID, video.TranscodeReady, hlsURL, hlsDir); err != nil {
		return err
	}

	// 磁盘占用一起报出来。这不是调试信息，是**验收项之一**：
	// "转码多档"这个优化的成本全在这里（一条 45 MB 的源变成三档 HLS 是
	// 多少 MB？占不占得起？），而它平时没有任何地方会显示。
	bytes, files := transcode.DirSize(hlsDir)
	log.Printf("[TranscodeWorker] video=%d 就绪 %d 档 耗时=%v 占用=%.1fMB/%d 文件 url=%s（%s）",
		videoID, tiers, time.Since(started).Round(time.Millisecond),
		float64(bytes)/(1<<20), files, hlsURL, why)
	return nil
}

// fail 记一次**终态**失败：写 failed，然后告诉上层"这条消息处理完了"。
//
// ---------- 为什么返回 nil（= Ack）而不是 error ----------
//
// 因为它叫"终态"。走到这里的每一种原因都是确定性的（源文件坏、路径不对、
// ffmpeg 报错、产物校验不过）—— 重试 4 次会拿到 4 个一模一样的失败，
// 而每次的代价是**分钟级的 CPU**。别处 worker 的重试之所以划算，
// 是因为它们失败一次只要几毫秒。
//
// 真正需要重试的情况（DB 连不上）走的是 `return err` 那条路，不经过这里。
//
// ---------- 为什么用 WithoutCancel ----------
//
// ctx 可能已经因为 Ctrl+C 被取消了，而"写下这个失败"恰恰是在取消路径上
// 最需要做成的一件事 —— 不做的话这条视频会**永远停在 running**，
// 而 running 的语义是"worker 正在处理它"，于是没有任何机制会再碰它。
// WithoutCancel 保留 ctx 的值（trace、超时之外的 deadline）但脱离取消。
//
// ⚠ 已知代价：写 failed 也失败时只记日志，库里停在旧状态（多半是 running）。
// 这是三选一里最不坏的一个 —— 另外两个是"重试（白烧 CPU）"和
// "panic（把整个 worker 进程带走）"。
func (w *TranscodeWorker) fail(ctx context.Context, videoID uint, format string, args ...any) error {
	reason := fmt.Sprintf(format, args...)
	log.Printf("[TranscodeWorker] video=%d 转码失败，直传原文件: %s", videoID, reason)

	if err := w.markTranscode(context.WithoutCancel(ctx), videoID,
		video.TranscodeFailed, "", ""); err != nil {
		log.Printf("[TranscodeWorker] video=%d 连 failed 状态都没写进去: %v（它会停在旧状态，注意 running 的孤儿）",
			videoID, err)
	}
	return nil
}

// sweep 清掉没有被当前 master 引用的旧运行目录。
//
// keep 从 master 里推导，**不是**从调用方记着的 runID ——
// 这样"保留哪一份"这件事只有一个来源（master），而 master 正是
// 播放器真正在读的那一份。调用方传 runID 的话，一旦某次路径上
// 忘记了更新它，删掉的就可能是正在被别人播的那一棵树。
//
// 失败只记日志：清理是**收尾**，让收尾失败去推翻一次成功的转码
// （触发重投 → 白重转一遍）是本末倒置。
func (w *TranscodeWorker) sweep(hlsRoot, masterPath string) {
	removed := transcode.CleanupRuns(hlsRoot, transcode.MasterRunIDs(masterPath))
	if removed > 0 {
		log.Printf("[TranscodeWorker] 清掉 %d 个旧运行目录（%s）", removed, hlsRoot)
	}
}

// tierNames 只为日志。档位名列表在排查时比档数有用
// （"3 档"看不出是哪三档；"1080p/720p/480p"一眼看得出源够不够高）。
func tierNames(tiers []video.Tier) []string {
	out := make([]string, 0, len(tiers))
	for _, t := range tiers {
		out = append(out, t.Name)
	}
	return out
}

// lowestHeight 最低档的短边像素数，只用在失败原因里。
//
// 不去调用方那边算（`video.DefaultTiers[len-1].Height`）是为了不假设
// "档位表按高到低排"这个顺序 —— 顺序变了的话，那句错误信息会
// 报一个错的数字，而错误信息里的数字是排查时唯一不去验证的东西。
func lowestHeight() int {
	lowest := 0
	for _, t := range video.DefaultTiers {
		if lowest == 0 || t.Height < lowest {
			lowest = t.Height
		}
	}
	return lowest
}
