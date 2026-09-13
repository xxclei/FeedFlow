package video_test

// 阶段4 的并发验收测试（对应 04-点赞模块.md 验收清单里那条
// "50 个 goroutine 同时点赞同一视频 → likes 表只有 1 行、likes_count == 1"）。
//
// 为什么是**外部测试包**（package video_test 而不是 package video）：
// internal/db 为了 AutoMigrate 已经 import 了 internal/video。测试如果写成
// package video 再 import internal/db，就构成 video → db → video 的导入环，
// 编译不过。外部测试包多一层，环就断开了。
//
// 跑法（config.yaml 是相对路径，go test 的 CWD 是包目录，所以往上退两级）：
//
//	cd myfeed && go test ./internal/video/ -run TestConcurrent -v
//
// 测试会自己 AutoMigrate（顺带把 likes 表建出来），建一条**专用**测试视频，
// 跑完在 t.Cleanup 里连带 likes 行一起删掉 —— 不碰任何真实数据。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"myfeed/internal/config"
	"myfeed/internal/db"
	"myfeed/internal/video"
)

const (
	concurrency = 50 // 并发度
	testAccount = 1  // 借用一个真实账号 id（表上没有 FK，用谁都行；借用是为了将来加了 FK 也不会挂）
)

// likeErr 是 service 在"已经赞过/还没赞过"时返回的文案，断言时按文案比而不是按 error 类型
// —— 这些错误是 errors.New 造的无类型错误，这也是本项目"业务错误一律 500"的代价。
var (
	errLiked    = "user has liked this video"
	errNotLiked = "user has not liked this video"
)

// setup 连库、建表、造一条专用测试视频，返回全套依赖。
func setup(t *testing.T) (*gorm.DB, *video.LikeRepository, *video.VideoRepository, *video.LikeService, uint) {
	t.Helper()

	cfg, err := config.Load("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("加载配置失败（CWD 必须能退到 myfeed/）: %v", err)
	}
	gormDB, err := db.NewDB(cfg.Database)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	if err := db.AutoMigrate(gormDB); err != nil {
		t.Fatalf("自动建表失败: %v", err)
	}

	v := &video.Video{
		AuthorID: testAccount,
		Username: "concurrency-test",
		Title:    fmt.Sprintf("concurrency-test-%d", time.Now().UnixNano()),
		PlayURL:  "",
		CoverURL: "",
	}
	if err := gormDB.Create(v).Error; err != nil {
		t.Fatalf("造测试视频失败: %v", err)
	}
	t.Cleanup(func() {
		gormDB.Exec("DELETE FROM likes WHERE video_id = ?", v.ID)
		gormDB.Exec("DELETE FROM videos WHERE id = ?", v.ID)
	})

	likeRepo := video.NewLikeRepository(gormDB)
	videoRepo := video.NewVideoRepository(gormDB)
	return gormDB, likeRepo, videoRepo, video.NewLikeService(likeRepo, videoRepo), v.ID
}

// runConcurrently 让 n 个 goroutine 在**同一条起跑线**上同时出发。
//
// 这里的 start channel 不是装饰：不写它的话，goroutine 会被调度器和连接池
// 错开成事实上的串行，"并发测试"就什么都没测到（全在预检那儿被挡掉了）。
func runConcurrently(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

// tally 把 n 个结果分成五类，**并把预期外的错误直接报成测试失败**。
//
// allowed 收的是"允许出现的业务错误文案"（errLiked / errNotLiked）。
// 用文案比对不好看，但错误本身是 errors.New 造的无类型错误 —— 本项目
// "业务错误一律 500"的代价就是错误不可分类，只能比字符串。这个测试顺手把
// 这件事的痛感记下来了。
//
// 返回值：success 成功数；conflict 数据库冲突数（两者都意味着事务被中止）；
// biz 命中的预期业务错误数。
func tally(t *testing.T, errs []error, allowed ...string) (success, conflict, biz int) {
	t.Helper()
	var dup, deadlock, other int
	for _, err := range errs {
		switch {
		case err == nil:
			success++
		case isDupKeyTest(err):
			dup++
		case isDeadlockTest(err):
			// InnoDB 在多个事务抢同一把唯一键锁时可能报 1213（死锁）而不是 1062。
			// 这不是 bug，是 InnoDB 对"并发插入同一个唯一键"的标准行为：
			// 每个插入者都要在唯一索引上拿 S 锁做重复检查，多个插入者互相等
			// 就会成环，MySQL 挑一个牺牲掉。
			//
			// 两种结果对**数据正确性**是等价的：事务都被中止、都回滚了，
			// 计数一步都没落盘。区别在**客户端体验**：1062 是"你已经赞过了"
			// （400/业务错误，不该重试），1213 是"这次没成功"（500，该重试）。
			// 阶段9 的 MQ 重试通道正是为这类偶发失败准备的。
			deadlock++
		case slices.Contains(allowed, err.Error()):
			biz++
		default:
			other++
			t.Errorf("意料之外的错误: %v", err)
		}
	}
	conflict = dup + deadlock
	t.Logf("结果分布: 成功 %d / 冲突 %d（1062:%d 1213死锁:%d） / 预期业务错误 %d / 其它 %d",
		success, conflict, dup, deadlock, biz, other)
	return success, conflict, biz
}

func isDupKeyTest(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func isDeadlockTest(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1213
}

// assertCounts 直接读库，检查两个冗余计数器。**不走缓存、不走对象**——
// 要验的就是落盘的值。
func assertCounts(t *testing.T, gormDB *gorm.DB, videoID uint, wantLikes, wantPop int64) {
	t.Helper()
	var v video.Video
	if err := gormDB.First(&v, videoID).Error; err != nil {
		t.Fatalf("回查视频失败: %v", err)
	}
	if v.LikesCount != wantLikes {
		t.Errorf("likes_count = %d，期望 %d", v.LikesCount, wantLikes)
	}
	if v.Popularity != wantPop {
		t.Errorf("popularity = %d，期望 %d", v.Popularity, wantPop)
	}
}

// assertNoDrift 是本模块最该长期盯住的不变量：**计数是流水的派生值**。
// 两张表对不上 = 冗余计数漂移了，而漂移只能用流水重算来修。
func assertNoDrift(t *testing.T, gormDB *gorm.DB, videoID uint) {
	t.Helper()
	var rows int64
	if err := gormDB.Model(&video.Like{}).Where("video_id = ?", videoID).Count(&rows).Error; err != nil {
		t.Fatalf("统计流水失败: %v", err)
	}
	var v video.Video
	if err := gormDB.First(&v, videoID).Error; err != nil {
		t.Fatalf("回查视频失败: %v", err)
	}
	if v.LikesCount != rows {
		t.Errorf("计数漂移: likes 表 %d 行，videos.likes_count = %d", rows, v.LikesCount)
	}
	if v.Popularity != rows {
		t.Errorf("计数漂移: likes 表 %d 行，videos.popularity = %d", rows, v.Popularity)
	}
}

// ---------- 测试一：service 全路径 ----------

// TestConcurrentLike_ServiceLevel 50 个 goroutine 走完整的 service.Like。
// 绝大多数会被"已赞预检"挡掉，所以它主要验**响应侧的正确性**：
// 恰好 1 个成功、其余全是"你已经赞过了"，库里 1 行、计数 1。
func TestConcurrentLike_ServiceLevel(t *testing.T) {
	gormDB, _, _, svc, videoID := setup(t)

	errs := runConcurrently(concurrency, func(i int) error {
		return svc.Like(context.Background(), &video.Like{VideoID: videoID, AccountID: testAccount})
	})

	success, _, biz := tally(t, errs, errLiked)
	if success != 1 {
		t.Errorf("成功数 = %d，期望恰好 1", success)
	}
	// 其余 49 个必须**全部**是被预检挡下的"你已经赞过了"。
	// 如果这里出现 1062，说明预检没起作用（并发窗口被撞穿了）—— 那也不算错，
	// 只是说明这一轮比预期更接近真实竞争；但 biz+conflict 必须凑满 49。
	if biz != concurrency-1 {
		t.Logf("注意：有 %d 个请求是撞到数据库唯一索引（而不是被预检挡下）才失败的",
			concurrency-1-biz)
	}
	assertCounts(t, gormDB, videoID, 1, 1)
	assertNoDrift(t, gormDB, videoID)
}

// ---------- 测试二：防线本身 ----------

// TestConcurrentLike_ConstraintLayer **绕过 service 的全部预检**，
// 50 个 goroutine 直接抢同一行的 INSERT + 两条 UPDATE。
//
// 这才是唯一索引 + 事务边界真正的考试：测试一里被预检挡掉的请求根本没到数据库，
// 而线上真正危险的是"两个请求同时通过了预检"。
//
// 断言里最值钱的一条是 likes_count == 1：49 个撞键的事务必须**一步都没落盘**。
// 注意这里测的是"唯一索引 + 事务回滚"这一层；"计数逃逸出事务"是另一个坑，
// 而且**这个测试的顺序测不出它** —— 因为 1062 发生在计数之前，闭包提前 return，
// 后面两条 UPDATE 根本没执行。那个坑的确定性复现是
// TestTransactionEscape_DemonstratesDrift。
func TestConcurrentLike_ConstraintLayer(t *testing.T) {
	gormDB, likeRepo, videoRepo, _, videoID := setup(t)

	errs := runConcurrently(concurrency, func(i int) error {
		return likeRepo.Transaction(context.Background(), func(tx *gorm.DB) error {
			err := likeRepo.LikeTx(tx, &video.Like{
				VideoID:   videoID,
				AccountID: testAccount,
				CreatedAt: time.Now(),
			})
			if err != nil {
				return err // 1062 原样上抛 → 事务回滚
			}
			if err := videoRepo.ChangeLikesCountTx(tx, videoID, +1); err != nil {
				return err
			}
			return videoRepo.ChangePopularityTx(tx, videoID, +1)
		})
	})

	success, conflict, _ := tally(t, errs)
	if success != 1 {
		t.Errorf("成功数 = %d，期望恰好 1", success)
	}
	if success+conflict != concurrency {
		t.Errorf("成功+冲突 = %d，期望 %d", success+conflict, concurrency)
	}
	// 关键：49 个失败的事务必须**一步都没落盘**
	assertCounts(t, gormDB, videoID, 1, 1)
	assertNoDrift(t, gormDB, videoID)
}

// ---------- 测试三：反向（取消点赞）不能把计数打成负数 ----------

// TestConcurrentUnlike_NoNegative 先赞一次，再 50 个并发取消。
// 验两件事：GREATEST(x-1, 0) 保证不出负数，DeleteByVideoAndAccountTx 的
// RowsAffected 保证"同一次取消不会被减两遍"（后者 GREATEST 挡不住）。
func TestConcurrentUnlike_NoNegative(t *testing.T) {
	gormDB, _, _, svc, videoID := setup(t)
	ctx := context.Background()

	if err := svc.Like(ctx, &video.Like{VideoID: videoID, AccountID: testAccount}); err != nil {
		t.Fatalf("准备阶段点赞失败: %v", err)
	}
	assertCounts(t, gormDB, videoID, 1, 1)

	errs := runConcurrently(concurrency, func(i int) error {
		return svc.Unlike(ctx, &video.Like{VideoID: videoID, AccountID: testAccount})
	})

	success, _, biz := tally(t, errs, errNotLiked)
	if success != 1 {
		t.Errorf("成功数 = %d，期望恰好 1", success)
	}
	if biz != concurrency-1 {
		t.Logf("注意：有 %d 个取消失败是撞到 RowsAffected=0（而不是被预检挡下）",
			concurrency-1-biz)
	}
	// 0 而不是负数：GREATEST 挡住负数，RowsAffected 挡住"同一次取消减两遍"
	assertCounts(t, gormDB, videoID, 0, 0)
	assertNoDrift(t, gormDB, videoID)
}

// ---------- 测试四：并发"赞-取消-赞"混合，最终态必须自洽 ----------

// TestConcurrentLikeUnlike_Mixed 每组 goroutine 做一次 like 再一次 unlike，
// 中间不加任何同步。最终哪个请求赢是随机的，但**不变量必须成立**：
// likes 表 0 或 1 行，且两个计数严格等于行数。
// 这个测试不预设结果 —— 它验的是"任何交错顺序下都不会漂移"。
func TestConcurrentLikeUnlike_Mixed(t *testing.T) {
	gormDB, _, _, svc, videoID := setup(t)
	ctx := context.Background()

	errs := runConcurrently(concurrency, func(i int) error {
		like := &video.Like{VideoID: videoID, AccountID: testAccount}
		_ = svc.Like(ctx, like) // Like 的错误不看：这一轮里它成功失败都正常
		return svc.Unlike(ctx, &video.Like{VideoID: videoID, AccountID: testAccount})
	})
	// 两种文案都允许出现：谁先谁后是随机的，"赞到一半被别人的取消插队"是合法结果
	tally(t, errs, errLiked, errNotLiked)

	// 只断言自洽性：谁赢都行，但不能漂
	assertNoDrift(t, gormDB, videoID)

	var rows int64
	gormDB.Model(&video.Like{}).Where("video_id = ?", videoID).Count(&rows)
	if rows > 1 {
		t.Errorf("likes 表 %d 行，一人一赞的唯一索引没生效", rows)
	}
	t.Logf("混合交错结束: likes 表 %d 行，计数已与流水一致", rows)
}

// ---------- 测试五：事务逃逸的确定性复现（反面教材） ----------

// TestTransactionEscape_DemonstratesDrift 故意复现"计数逃逸出事务"的错误写法。
// 两个子测试**只差一个函数名**（ChangeLikesCount / ChangeLikesCountTx），
// 结果一个是 likes_count = 1，另一个是 0。
//
// 复现的前提是一个容易被忽略的条件：**失败的那一步必须排在逃逸的写入之后**。
// 计数先独立提交、后面的步骤再失败并触发回滚，才会留下错数据。
// 如果失败排在计数之前，闭包提前 return，逃逸的代码根本执行不到 ——
// 当前 service 恰好就是这种"运气好的顺序"（先插流水、后改计数），
// 所以线上这一版是安全的；但阶段9 要在计数之后投递 MQ 消息，顺序就变了。
//
// 这个测试同时是这套设计的说明书：它证明"把 tx 做成显式参数"不是洁癖，
// 而是在堵一个真实存在的、而且**不报任何错**的口子。
func TestTransactionEscape_DemonstratesDrift(t *testing.T) {
	ctx := context.Background()
	// 注入一个"计数写完之后才发生"的失败，模拟三种真实情形之一：
	// COMMIT 阶段失败 / 第二条 UPDATE 拿不到锁超时 / 阶段9 的 MQ 投递失败。
	injected := errors.New("注入的失败（模拟 COMMIT 失败 · 锁等待超时 · MQ 投递失败）")

	t.Run("错误写法_非事务版_计数漂移", func(t *testing.T) {
		gormDB, likeRepo, videoRepo, _, videoID := setup(t)

		err := likeRepo.Transaction(ctx, func(tx *gorm.DB) error {
			// 传进去的 tx 被**忽略**：这两个方法绑的是 vr.db，SQL 跑在另一条连接上
			// 并立刻提交。事务后面回滚，跟它们没关系。
			if err := videoRepo.ChangeLikesCount(ctx, videoID, +1); err != nil {
				return err
			}
			if err := videoRepo.ChangePopularity(ctx, videoID, +1); err != nil {
				return err
			}
			if err := likeRepo.LikeTx(tx, &video.Like{
				VideoID: videoID, AccountID: testAccount, CreatedAt: time.Now(),
			}); err != nil {
				return err
			}
			return injected // ← 后置失败，事务从这里回滚
		})
		if err == nil {
			t.Errorf("期望注入的错误被返回，却得到 nil（事务居然成功了）")
		} else {
			t.Logf("事务按预期失败并回滚: %v", err)
		}

		var rows int64
		gormDB.Model(&video.Like{}).Where("video_id = ?", videoID).Count(&rows)
		var v video.Video
		gormDB.First(&v, videoID)
		t.Logf("→ likes 表 %d 行（流水回滚干净），likes_count = %d（计数没回滚）", rows, v.LikesCount)

		if rows != 0 {
			t.Errorf("likes 表 %d 行，期望 0 —— 事务应该把流水回滚掉", rows)
		}
		if v.LikesCount != 1 {
			t.Errorf("likes_count = %d，期望 1 —— 漂移应然发生", v.LikesCount)
		}
	})

	t.Run("正确写法_事务版_一起回滚", func(t *testing.T) {
		gormDB, likeRepo, videoRepo, _, videoID := setup(t)

		err := likeRepo.Transaction(ctx, func(tx *gorm.DB) error {
			// 唯一的区别：传 tx。写进同一个事务，跟流水同生共死。
			if err := videoRepo.ChangeLikesCountTx(tx, videoID, +1); err != nil {
				return err
			}
			if err := videoRepo.ChangePopularityTx(tx, videoID, +1); err != nil {
				return err
			}
			if err := likeRepo.LikeTx(tx, &video.Like{
				VideoID: videoID, AccountID: testAccount, CreatedAt: time.Now(),
			}); err != nil {
				return err
			}
			return injected
		})
		if err == nil {
			t.Errorf("期望注入的错误被返回，却得到 nil")
		}

		var rows int64
		gormDB.Model(&video.Like{}).Where("video_id = ?", videoID).Count(&rows)
		var v video.Video
		gormDB.First(&v, videoID)
		t.Logf("→ likes 表 %d 行，likes_count = %d（两个都被回滚）", rows, v.LikesCount)

		if rows != 0 {
			t.Errorf("likes 表 %d 行，期望 0", rows)
		}
		if v.LikesCount != 0 {
			t.Errorf("likes_count = %d，期望 0 —— 计数必须跟着流水一起回滚", v.LikesCount)
		}
	})
}
