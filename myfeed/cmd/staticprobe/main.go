// staticprobe —— 只用来量静态文件分发路径的探针服务。
//
// 为什么需要它：myfeed.exe 的启动**硬依赖 MySQL**
// （cmd/main.go 里 `log.Fatalf("连接数据库失败: %v", err)`），
// 而 Docker 没运行时 MySQL/Redis/RabbitMQ 全都不在。
// 但要量的三件事（首帧时间 / 并发上限 / 服务端吞吐）**和 DB 一点关系都没有** ——
// 它们全部发生在 `r.Static` 这条路上：
//
//	gin.Static → http.FileServer → http.ServeContent → io.Copy(w, file)
//
// 所以这里把那条路**原样搬出来**：下面那行 r.Static 和
// internal/http/router.go 里的调用**逐字相同**，参数也一样。
//
// ⚠ 诚实边界：这是**替身**，不是真服务。它没有 SSE 长连接、没有 outbox 轮询，
// 所以量出来的"-服务端能不能扛住"是**上界**，真实服务只会更差一点。
// 换句话说：如果探针都扛不住，真服务肯定扛不住；探针扛得住，真服务还要再验一次。
//
// 用法：
//
//	cd myfeed && go run ./cmd/staticprobe [addr]
//	默认监听 :8099
package main

import (
	"log"
	"net/http"
	"os"

	"myfeed/internal/config"

	"github.com/gin-gonic/gin"
)

func main() {
	addr := ":8099"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// ⬇⬇ 这一行和 internal/http/router.go 里那条**解出来的实际目录相同** ⬇⬇
	//
	// router.go 那边写的是 cfg.Storage.Root()（读 config.yaml，没写就回落默认值）；
	// 这里**故意不读 config.yaml** —— 这个探针存在的意义就是"DB 没起来也能量"，
	// 让它去依赖一个配置文件是没必要的耦合。代价是：
	//
	//	⚠ 如果你的 config.yaml 改过 storage.upload_root，
	//	  这个探针量的就不是真服务在用的那个目录。改的时候两边一起改。
	r.Static("/static", config.DefaultUploadRoot)

	log.Printf("[staticprobe] listening on %s, serving %s at /static", addr, config.DefaultUploadRoot)
	if err := r.Run(addr); err != nil {
		log.Fatalf("[staticprobe] %v", err)
	}
}
