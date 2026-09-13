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

// InitChunkUpload ① 开会话：查断点续传索引 → 有就接着传，没有就新建。
// 新建时顺带把最终文件和 .part 半成品都预分配好，后续分片直接往偏移上写
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

	if req.FileSize > maxUploadSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file size exceeds 500MB limit"})
		return
	}
	if err := validateChunkPlan(req.FileSize, req.ChunkSize, req.TotalChunks); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 断点续传：同账号同 file_hash 的会话还活着 → 直接续（RefreshTTL 阶段7回填）
	if session, ok := h.store.getByHash(accountID, req.FileHash); ok {
		// 分片几何必须完全一致才能续：磁盘上已写的分片是按**旧** chunk_size 落的偏移，
		// 档位一变就对不上了（前端改分档规则后重传同一个文件正好会走到这里）。
		// 与其返回错误把这个文件钉死，不如丢掉旧会话重开一个 —— 分片本来就要重传。
		if session.FileSize == req.FileSize &&
			session.ChunkSize == req.ChunkSize &&
			session.TotalChunks == req.TotalChunks {
			c.JSON(http.StatusOK, gin.H{
				"upload_id":       session.UploadID,
				"uploaded_chunks": session.UploadedChunks(),
			})
			return
		}
		h.discard(session)
	}

	// 输出路径在 init 就定下来：分片直接按偏移写进最终文件，必须先有归宿。
	// 目录结构和直传保持一致（videos/<作者ID>/<日期>/），避免单目录塞几十万文件
	date := time.Now().Format("20060102")
	relDir := filepath.Join("videos", fmt.Sprintf("%d", accountID), date)
	absDir := filepath.Join(".run", "uploads", relDir)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create output dir"})
		return
	}

	filename, err := randHex(16)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate filename"})
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
		FinalPath:    filepath.Join(absDir, filename+".mp4"),
		URLPath:      fmt.Sprintf("/static/videos/%d/%s/%s.mp4", accountID, date, filename),
	}

	// 预先把文件撑到最终大小（Truncate 只移动 EOF，不写零，所以不费 IO）。
	// 之后每片按 index*chunk_size 直接写到自己的位置上，complete 那一步就没有"合并"了
	if err := preallocate(session.partPath(), req.FileSize); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upload file"})
		return
	}

	h.store.put(session)

	c.JSON(http.StatusOK, gin.H{
		"upload_id":       uploadID,
		"uploaded_chunks": []int{},
	})
}

// discard 丢掉一个不能再用的会话：断点续传索引、位图、半成品文件一起清。
// 不丢的话，半成品 .part 会永远留在磁盘上（完不成的会话没人再来收尾）
func (h *ChunkUploadHandler) discard(s ChunkUploadSession) {
	h.store.remove(s.UploadID)
	_ = os.Remove(s.partPath())
}

// preallocate 建好文件并把长度定死在 size
func preallocate(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
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

	// 这一片该有多少字节。客户端分片几何算错的话在这里就挡住，
	// 不必等到写歪文件、拉长成"视频能播但中间一段是花的"再回头查
	wantLen := session.chunkLen(req.ChunkIndex)
	if f.Size != wantLen {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "chunk size mismatch",
			"want":  wantLen,
			"got":   f.Size,
		})
		return
	}

	chunkFile, err := f.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read chunk"})
		return
	}
	defer chunkFile.Close()

	// 逐字节喂给 MD5（不整读进内存——分片最大 20MB，但这个习惯对大流很重要）
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

	if _, seekErr := chunkFile.Seek(0, io.SeekStart); seekErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read chunk"})
		return
	}

	// 直接写到最终文件的偏移上：index 决定落点，所以乱序到达、多片并发都无所谓。
	//
	// 每次 open 自己的 fd，而不是把 fd 挂在会话上：会话可能在 map 里躺 24 小时，
	// 常驻 fd 就是泄漏。多个 fd 各自 WriteAt 到不同偏移是安全的 ——
	// WriteAt 不带共享的文件位置指针（对应 pwrite），不存在互相踩 offset 的问题。
	dst, err := os.OpenFile(session.partPath(), os.O_WRONLY, 0o644)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to open target file"})
		return
	}
	defer dst.Close()

	// NewOffsetWriter 把「从 offset 开始顺序写」包装成普通 Write，
	// 于是能直接 io.Copy 流式落盘，不用把整片读进内存
	ow := io.NewOffsetWriter(dst, int64(req.ChunkIndex)*session.ChunkSize)
	n, err := io.Copy(ow, chunkFile)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write chunk"})
		return
	}
	if n != wantLen {
		// MD5 都对上了还写不满 → 是磁盘/环境的问题，不是客户端的问题，值得重试（返回 5xx）
		c.JSON(http.StatusInternalServerError, gin.H{"error": "short write"})
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

// CompleteChunkUpload ④ 收尾：全到齐 → 把 .part 改名成 .mp4 → 销毁会话。
// 没有"合并"这一步——分片早就写到自己该在的偏移上了
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

	partPath := session.partPath()

	// init 时已经 Truncate 到最终大小，所以 file_size 是构造上就成立的；
	// 这里 Stat 只是确认文件还在 —— 被外部删掉时要给出带 "missing" 的错误，
	// 前端才认得出来该走「加盐重开会话」的逃生舱（见 utils/chunkUploader.ts 的 isPoisoned）
	if _, err := os.Stat(partPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "chunk file missing"})
		return
	}

	// 预分配 + 偏移直写之后，「合并」就只剩一次改名：
	// 没有一次全量拷贝（老实现是读 500MB 再写 500MB），也没有中间目录要清。
	// 同目录内 rename 是原子的 —— 要么看到完整的 .mp4，要么什么都没有，不存在半成品
	if err := os.Rename(partPath, session.FinalPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize file"})
		return
	}

	// 清理两件套：会话、断点续传索引（分片目录已经不存在了）
	h.store.remove(req.UploadID)

	playURL := buildAbsoluteURL(c, session.URLPath)

	c.JSON(http.StatusOK, gin.H{
		"url":      playURL,
		"play_url": playURL,
	})
}
