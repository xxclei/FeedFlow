package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// **本轮状态：没有构造点**（router.go 里给 NewService 的 embed 传的是 nil）。
// 代码是完整的、可用的，只是还没接线 —— 见 search/entity.go 的"本轮状态"。
//
// Embedder 把文本变成向量。
//
// 接口而不是直接依赖 OllamaEmbedder：测试和本地开发需要一个"不用起 Ollama"的实现，
// 而且**换模型是本轮明说会发生的运营动作**（embedding_model 那两列存在的唯一理由）。
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Model 模型名。它会被写进 videos.embedding_model —— 换模型 = 全量重嵌，
	// 靠的就是这个名字比对（见 video_repo.go 的 ListMissingEmbedding）
	Model() string
	// Dim 向量维度。必须和 Redis 索引建的时候那个 DIM 一致
	Dim() int
}

// OllamaEmbedder 走 Ollama 的 HTTP API。
//
// ---------- 这是全项目第一处对外部服务的网络调用 ----------
//
// 之前所有依赖（MySQL / Redis / RabbitMQ）都是"基础设施"，这里第一次调用一个
// **推理服务**。两件事因此变得可能而以前不可能：
//
//	它很慢（一次 embedding 几百毫秒到几秒）
//	它会挂、会被卸载、会被 docker stop
//
// 所以它有两条自己的纪律，都在别处体现：
//   - 查询侧超时短（2s 量级），慢了就降级成纯词法搜索，不能把用户卡住
//   - **发布路径完全不碰它**（见 video_service.go 的 Publish）：异步补，丢了有回填兜底
type OllamaEmbedder struct {
	baseURL string
	model   string
	dim     int
	client  *http.Client
}

func NewOllamaEmbedder(baseURL, model string, dim int, timeout time.Duration) *OllamaEmbedder {
	return &OllamaEmbedder{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		dim:     dim,
		// 每个 embedder 一个自己的 client，而不是用 http.DefaultClient：
		// DefaultClient 没有超时（零值 = 永不超时），一个卡住的 embedding 请求
		// 会一直占着 goroutine。显式超时是这个项目对"外部依赖"的最低要求。
		client: &http.Client{Timeout: timeout},
	}
}

func (e *OllamaEmbedder) Model() string { return e.model }
func (e *OllamaEmbedder) Dim() int      { return e.dim }

// ollamaEmbedRequest /api/embed 的请求体。
//
// **注意字段是 `input`（数组）而不是 `prompt`，响应是 `embeddings`（复数）。
// ** 单数形式的 `/api/embeddings` + `prompt` 是旧接口，已经废弃 ——
// 打错了的表现是 404 或 400，很容易被误判成"Ollama 没起来"。
// `input` 收数组的好处是回填命令可以**批量**嵌（一次几十条），
// 而不是每条视频一次 HTTP 往返。
type ollamaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed 批量嵌入。texts 为空时直接返回，不发请求。
func (e *OllamaEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(ollamaEmbedRequest{Model: e.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("序列化 embedding 请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造 embedding 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用 Ollama 失败（%s）: %w", e.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 把响应体的一小段带进错误里：Ollama 的 404 通常是"模型没 pull 下来"、
		// 400 通常是"模型名不对"，而这两种的**状态码不一样但含义都是"配置问题"**，
		// 光看状态码分不清。截断到 512 字节是为了不把一整个 HTML 错误页塞进日志。
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("Ollama /api/embed 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var out ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("解析 Ollama 响应失败: %w", err)
	}

	// ---------- 下面两个校验不是防御性编程，是防一类静默的数据损坏 ----------
	//
	// ① 条数必须一一对应。少了或多了，**后面的向量会整体错位**：
	//    video #7 拿到 video #8 的向量。这件事不会报任何错，
	//    表现只是"搜 A 出来 B"，而且因为向量看起来都"有点道理"，很难归因。
	// ② 维度必须等于配置值。不等说明模型不是 bge-m3（bge-m3 是 1024 维）。
	//    维度不对的话写进 Redis 会被索引拒掉（DIM 不匹配），
	//    但那时错误信息是 RediSearch 的，指向的是写入那一步，
	//    不如在这里说清楚"是模型不对"。
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("Ollama 返回的向量条数 %d 与请求的文本条数 %d 不一致（会导致向量与视频错位）",
			len(out.Embeddings), len(texts))
	}
	for i, vec := range out.Embeddings {
		if len(vec) != e.dim {
			return nil, fmt.Errorf("第 %d 条向量维度是 %d，配置里是 %d —— 模型 %q 和配置对不上",
				i, len(vec), e.dim, e.model)
		}
	}
	return out.Embeddings, nil
}
