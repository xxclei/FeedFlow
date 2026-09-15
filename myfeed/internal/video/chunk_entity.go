package video

import "fmt"

// 单文件上限。直传（video_handler.go）和分片 init 两条路径**共用同一个值**，
// 改一处两边都跟着变，不会出现"直传放行 500MB 而分片卡 200MB"这种漂移。
const maxUploadSize = 500 << 20 // 500 MB

// ChunkSize 分片大小的最小档，也是老前端的固定值。
// 阶段7 起前端按文件大小分档（5/10/20MB），服务端**照单全收**客户端在 init 里
// 声明的 chunk_size —— 落盘偏移、每片长度、位图大小全部由它推导，所以这里不再假设一个固定值。
const ChunkSize = 5 << 20 // 5 MB

// 分片几何的上下界。不是用来限制用户的，是用来挡住畸形请求：
//   - 下界：chunk_size=1 配 500MB 文件 → total_chunks = 5 亿 → make([]bool, 5e8) 一口气吃 500MB 内存
//   - 上界：total_chunks 直接决定位图大小，必须有个硬顶
const (
	minChunkSize   = 1 << 20
	maxTotalChunks = 4096
)

// validateChunkPlan 校验分片几何自洽：total_chunks 必须正好等于 ceil(file_size / chunk_size)。
//
// 为什么现在是必需的：chunk_size 从"固定 5MB"变成"客户端按档位算出来的可变值"之后，
// 偏移、每片长度、位图大小全由它推导，一个前后矛盾的组合就会把文件写歪
// （chunk_size=1MB 却声明 total_chunks=1，写完只有前 1MB 是有内容的）。
// 以前靠"merge 时按序拼"掩盖了这类错误，现在必须显式挡住。
func validateChunkPlan(fileSize, chunkSize int64, totalChunks int) error {
	if chunkSize < minChunkSize {
		return fmt.Errorf("chunk_size too small (min %d bytes)", minChunkSize)
	}
	if chunkSize > maxUploadSize {
		return fmt.Errorf("chunk_size too large (max %d bytes)", maxUploadSize)
	}
	if totalChunks > maxTotalChunks {
		return fmt.Errorf("total_chunks too large (max %d)", maxTotalChunks)
	}
	// 用除法算期望片数而不是拿乘法比大小：乘法在 fileSize 逼近 int64 上限时会溢出。
	// 两个入参各自有上界，所以这里的加法不会溢出
	want := int((fileSize + chunkSize - 1) / chunkSize)
	if totalChunks != want {
		return fmt.Errorf("total_chunks mismatch: got %d, want %d (%d bytes / %d per chunk)",
			totalChunks, want, fileSize, chunkSize)
	}
	return nil
}

// ChunkUploadSession 一次分片上传的全部状态。
// 阶段2 存在内存 map 里；阶段7 整体迁到 Redis（结构不变，只换存储引擎）
type ChunkUploadSession struct {
	UploadID     string `json:"upload_id"`
	AccountID    uint   `json:"account_id"`
	Filename     string `json:"filename"`
	FileSize     int64  `json:"file_size"`
	ChunkSize    int64  `json:"chunk_size"`
	TotalChunks  int    `json:"total_chunks"`
	FileHash     string `json:"file_hash"`     // 整个文件的 MD5（前端算好带来的，断点续传的索引）
	UploadedBits []bool `json:"uploaded_bits"` // 位图：第 i 片是否已传

	// 下面两个在 init 时就定下来，不再等 complete 才生成：
	// 分片是按偏移**直接写进最终文件**的，必须先有一个确定的归宿。
	// 实际写的是 FinalPath + ".part"，complete 时才改名成 .mp4 ——
	// 上传期间不产生"可被访问到的半成品 mp4"，也就不需要临时分片目录。
	FinalPath string `json:"-"`
	URLPath   string `json:"-"` // /static/videos/<账号>/<日期>/<随机>.mp4
}

// partPath 落盘中的半成品路径
func (s *ChunkUploadSession) partPath() string { return s.FinalPath + ".part" }

// chunkLen 第 index 片应该有多少字节（最后一片可能不满）
func (s *ChunkUploadSession) chunkLen(index int) int64 {
	if index == s.TotalChunks-1 {
		return s.FileSize - int64(index)*s.ChunkSize
	}
	return s.ChunkSize
}

// UploadedChunks 已传分片的下标列表（断点续传时告诉前端"只补这些之外的"）
//
// ⚠ 返回的**永远**是 []int 而不是 nil —— 哪怕一片都没传。
//
// 这不是风格问题：nil slice 会被 encoding/json 编成 `null`（不是 `[]`），
// 而前端拿到 null 就是 `init.uploaded_chunks.reduce(...)` 直接崩。
// 2026-09-15 上云当天就是这么炸的：
//
//	TypeError: Cannot read properties of null (reading 'reduce')
//
// 触发路径很窄但很真实：**会话建好了、却一片都没落到盘上**（第一次传就断了），
// 然后重试。重试时 InitChunkUpload 会命中断点续传分支（file_hash 相同、
// 分片几何一致），走的就是这个函数 —— 此时 UploadedBits 全是 false，
// 下面那个 append **一次都没执行**，于是 `var indices []int` 原样返回 nil。
//
// 注意同一个 handler 里"新建会话"那条路（chunk_handler.go:200）特意写了
// `[]int{}`，说明这个坑当初是**知道**的 —— 只是没落在函数内部。
// "知道这件事"和"每一个返回点都做对"是两件事；写进函数里，两个调用点
// （chunk_handler.go:151 / :360）就同时对了。
func (s *ChunkUploadSession) UploadedChunks() []int {
	indices := make([]int, 0, len(s.UploadedBits))
	for i, uploaded := range s.UploadedBits {
		if uploaded {
			indices = append(indices, i)
		}
	}
	return indices
}

// IsComplete 所有片都到齐了才能合并
func (s *ChunkUploadSession) IsComplete() bool {
	for _, b := range s.UploadedBits {
		if !b {
			return false
		}
	}
	return true
}

// clone 深拷贝（UploadedBits 是切片，浅拷贝会共享底层数组）。
// 内存存储在锁内返回副本，避免外部读到写到一半的位图
func (s *ChunkUploadSession) clone() ChunkUploadSession {
	c := *s
	bits := make([]bool, len(s.UploadedBits))
	copy(bits, s.UploadedBits)
	c.UploadedBits = bits
	return c
}

// ---- 请求/响应结构 ----

type InitChunkUploadRequest struct {
	Filename    string `json:"filename" binding:"required"`
	FileSize    int64  `json:"file_size" binding:"required,min=1"`
	ChunkSize   int64  `json:"chunk_size" binding:"required,min=1"`
	TotalChunks int    `json:"total_chunks" binding:"required,min=1"`
	FileHash    string `json:"file_hash" binding:"required"`
}

// UploadChunk 是 multipart 表单 + 查询参数混合体，所以用 ShouldBind
type UploadChunkRequest struct {
	UploadID   string `form:"upload_id" binding:"required"`
	ChunkIndex int    `form:"chunk_index" binding:"min=0"`
	ChunkHash  string `form:"chunk_hash" binding:"required"`
}

type ChunkStatusRequest struct {
	UploadID string `json:"upload_id" binding:"required"`
}

type CompleteChunkUploadRequest struct {
	UploadID string `json:"upload_id" binding:"required"`
}
