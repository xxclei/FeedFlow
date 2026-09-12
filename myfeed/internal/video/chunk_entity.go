package video

const ChunkSize = 5 << 20 // 5 MB（前端默认分片大小）

// ChunkUploadSession 一次分片上传的全部状态。
// 阶段2 存在内存 map 里；阶段7 整体迁到 Redis（结构不变，只换存储引擎）
type ChunkUploadSession struct {
	UploadID     string `json:"upload_id"`
	AccountID    uint   `json:"account_id"`
	Filename     string `json:"filename"`
	FileSize     int64  `json:"file_size"`
	ChunkSize    int64  `json:"chunk_size"`
	TotalChunks  int    `json:"total_chunks"`
	FileHash     string `json:"file_hash"`      // 整个文件的 MD5（前端算好带来的，断点续传的索引）
	UploadedBits []bool `json:"uploaded_bits"`  // 位图：第 i 片是否已传
}

// UploadedChunks 已传分片的下标列表（断点续传时告诉前端"只补这些之外的"）
func (s *ChunkUploadSession) UploadedChunks() []int {
	var indices []int
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
