package video

import (
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"myfeed/internal/middleware/jwt"

	"github.com/gin-gonic/gin"
)

// 阶段2：会话存内存（进程重启即失忆，且无自动过期——两条已知局限）。
// 阶段7：原项目用 Redis 存会话（chunk_upload:<uploadID>，24h TTL），
// 换存储时只需重写下面 chunkSessionStore 的五个方法，handler 逻辑零改动
const sessionTTL = 24 * time.Hour // 仅为语义占位，内存版暂不做过期清理

var errChunkSessionNotFound = errors.New("upload session not found")

// chunkSessionStore 内存会话存储。
// sessions: uploadID → 会话；hashes: "账号ID:文件MD5" → uploadID（断点续传的索引）
type chunkSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*ChunkUploadSession
	hashes   map[string]string
}

func newChunkSessionStore() *chunkSessionStore {
	return &chunkSessionStore{
		sessions: make(map[string]*ChunkUploadSession),
		hashes:   make(map[string]string),
	}
}

func hashIndex(accountID uint, fileHash string) string {
	return fmt.Sprintf("%d:%s", accountID, fileHash)
}

// get 返回会话副本（锁内拷贝，外部随便用不担心竞态）
func (st *chunkSessionStore) get(uploadID string) (ChunkUploadSession, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[uploadID]
	if !ok {
		return ChunkUploadSession{}, false
	}
	return s.clone(), true
}

func (st *chunkSessionStore) getByHash(accountID uint, fileHash string) (ChunkUploadSession, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	uploadID, ok := st.hashes[hashIndex(accountID, fileHash)]
	if !ok {
		return ChunkUploadSession{}, false
	}
	s, ok := st.sessions[uploadID]
	if !ok {
		return ChunkUploadSession{}, false
	}
	return s.clone(), true
}

func (st *chunkSessionStore) put(s *ChunkUploadSession) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sessions[s.UploadID] = s
	st.hashes[hashIndex(s.AccountID, s.FileHash)] = s.UploadID
}

// mark 原子地标记某片已完成（并发传不同片不打架）
func (st *chunkSessionStore) mark(uploadID string, index int) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[uploadID]
	if !ok {
		return errChunkSessionNotFound
	}
	if index < 0 || index >= len(s.UploadedBits) {
		return errors.New("invalid chunk_index")
	}
	s.UploadedBits[index] = true
	return nil
}

// remove 会话 + 反查索引一起清（complete 成功后）
func (st *chunkSessionStore) remove(uploadID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if s, ok := st.sessions[uploadID]; ok {
		delete(st.hashes, hashIndex(s.AccountID, s.FileHash))
	}
	delete(st.sessions, uploadID)
}

// ---- HTTP handlers ----

type ChunkUploadHandler struct {
	store *chunkSessionStore
}

func NewChunkUploadHandler() *ChunkUploadHandler {
	return &ChunkUploadHandler{store: newChunkSessionStore()}
}

// InitChunkUpload ① 开会话：查断点续传索引 → 有就接着传，没有就新建
func (h *ChunkUploadHandler) InitChunkUpload(c *gin.Context) {
	var req InitChunkUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	const maxSize = 200 << 20 // 和直传一致的 200MB 上限
	if req.FileSize > maxSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file size exceeds 200MB limit"})
		return
	}

	// 断点续传：同账号同 file_hash 的会话还活着 → 直接续（RefreshTTL 阶段7回填）
	if session, ok := h.store.getByHash(accountID, req.FileHash); ok {
		c.JSON(http.StatusOK, gin.H{
			"upload_id":       session.UploadID,
			"uploaded_chunks": session.UploadedChunks(),
		})
		return
	}

	id, _ := randHex(16)
	uploadID := id + fmt.Sprintf("%d", time.Now().UnixNano()) // 随机数+纳秒时间戳，双重防撞
	session := &ChunkUploadSession{
		UploadID:     uploadID,
		AccountID:    accountID,
		Filename:     req.Filename,
		FileSize:     req.FileSize,
		ChunkSize:    req.ChunkSize,
		TotalChunks:  req.TotalChunks,
		FileHash:     req.FileHash,
		UploadedBits: make([]bool, req.TotalChunks), // 位图：全 false 起步
	}
	h.store.put(session)

	c.JSON(http.StatusOK, gin.H{
		"upload_id":       uploadID,
		"uploaded_chunks": []int{},
	})
}

// UploadChunk ② 收一片：验身份 → 验下标 → 幂等检查 → MD5校验 → 落盘 → 记位图
func (h *ChunkUploadHandler) UploadChunk(c *gin.Context) {
	var req UploadChunkRequest
	if err := c.ShouldBind(&req); err != nil { // multipart+query 混合体，用 ShouldBind
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	session, ok := h.store.get(req.UploadID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if session.AccountID != accountID {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"}) // 不是你的会话，别碰
		return
	}

	if req.ChunkIndex < 0 || req.ChunkIndex >= session.TotalChunks {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chunk_index"})
		return
	}

	// 幂等：这片已经传过 → 直接成功（重试场景不重复写）
	if session.UploadedBits[req.ChunkIndex] {
		c.JSON(http.StatusOK, gin.H{"chunk_index": req.ChunkIndex})
		return
	}

	f, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file"})
		return
	}

	chunkFile, err := f.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read chunk"})
		return
	}
	defer chunkFile.Close()

	// 逐字节喂给 MD5（不整读进内存——分片最大 5MB，但这个习惯对大流很重要）
	hash := md5.New()
	if _, err := io.Copy(hash, chunkFile); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash chunk"})
		return
	}
	actualHash := fmt.Sprintf("%x", hash.Sum(nil))

	// 完整性校验：客户端算的和实际收到的不一致 → 拒收并给出双哈希便于排查
	if actualHash != req.ChunkHash {
		c.JSON(http.StatusBadRequest, gin.H{"error": "chunk hash mismatch", "expected": req.ChunkHash, "actual": actualHash})
		return
	}

	// 分片先落 tmp/<uploadID>/<index>，全部到齐后才合并
	tmpDir := filepath.Join(".run", "uploads", "tmp", req.UploadID)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create temp dir"})
		return
	}

	chunkPath := filepath.Join(tmpDir, fmt.Sprintf("%d", req.ChunkIndex))
	if _, seekErr := chunkFile.Seek(0, io.SeekStart); seekErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read chunk"})
		return
	}

	dst, err := os.Create(chunkPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save chunk"})
		return
	}
	defer dst.Close()

	if _, err := io.Copy(dst, chunkFile); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save chunk"})
		return
	}

	if err := h.store.mark(req.UploadID, req.ChunkIndex); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update session"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"chunk_index": req.ChunkIndex})
}

// ChunkStatus ③ 查进度：断线重连后前端问"哪些片缺着"
func (h *ChunkUploadHandler) ChunkStatus(c *gin.Context) {
	var req ChunkStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	session, ok := h.store.get(req.UploadID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if session.AccountID != accountID {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"upload_id":       session.UploadID,
		"uploaded_chunks": session.UploadedChunks(),
		"total_chunks":    session.TotalChunks,
	})
}

// CompleteChunkUpload ④ 合并：全到齐 → 按 index 升序拼成 mp4 → 清理现场
func (h *ChunkUploadHandler) CompleteChunkUpload(c *gin.Context) {
	var req CompleteChunkUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	session, ok := h.store.get(req.UploadID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if session.AccountID != accountID {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}

	// 没传完就 complete → 拒绝，并告知缺口（missing 封顶 5：错误信息别无限长）
	if !session.IsComplete() {
		missing := 0
		for _, uploaded := range session.UploadedBits {
			if !uploaded {
				missing++
				if missing > 5 {
					break
				}
			}
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"error":     "not all chunks uploaded",
			"missing":   missing,
			"completed": len(session.UploadedChunks()),
			"total":     session.TotalChunks,
		})
		return
	}

	date := time.Now().Format("20060102")
	relDir := filepath.Join("videos", fmt.Sprintf("%d", accountID), date)
	root := filepath.Join(".run", "uploads")
	absDir := filepath.Join(root, relDir)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create output dir"})
		return
	}

	filename, err := randHex(16)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate filename"})
		return
	}
	finalPath := filepath.Join(absDir, filename+".mp4")

	finalFile, err := os.Create(finalPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create final file"})
		return
	}

	// 按 index 升序逐片拼入（分片乱序到达也没关系，index 决定顺序）
	tmpDir := filepath.Join(".run", "uploads", "tmp", req.UploadID)
	for i := 0; i < session.TotalChunks; i++ {
		chunkPath := filepath.Join(tmpDir, fmt.Sprintf("%d", i))
		cf, err := os.Open(chunkPath)
		if err != nil {
			finalFile.Close()
			os.Remove(finalPath) // 合并失败绝不留半成品
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("chunk %d missing", i)})
			return
		}
		_, err = io.Copy(finalFile, cf)
		cf.Close()
		if err != nil {
			finalFile.Close()
			os.Remove(finalPath)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to merge chunks"})
			return
		}
	}
	finalFile.Close()

	// 清理三件套：临时分片目录、会话、断点续传索引
	_ = os.RemoveAll(tmpDir)
	h.store.remove(req.UploadID)

	urlPath := fmt.Sprintf("/static/videos/%d/%s/%s.mp4", accountID, date, filename)
	playURL := buildAbsoluteURL(c, urlPath)

	c.JSON(http.StatusOK, gin.H{
		"url":      playURL,
		"play_url": playURL,
	})
}
