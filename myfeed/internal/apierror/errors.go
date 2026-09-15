package apierror

import (
	"encoding/json"
	"errors"
	"net/http"

	"gorm.io/gorm"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrValidation   = errors.New("validation error")

	// ErrForbidden 已认证但无权操作（阶段5 加）。
	// 和 ErrUnauthorized 的分工是"没登录"和"登录了但不是你"：
	//
	//	ErrUnauthorized = 401 —— 你的凭证无效/缺失（未登录、token 过期）
	//	ErrForbidden    = 403 —— 你确实是本人，但这件事不归你做（删别人的评论）
	//
	// 为什么非要分开：前端 handleResponse 在**任何 401** 上都会 auth.clearTokens()
	// （那是对的 —— 401 的意思就是"你的票不认了，重来吧"）。如果"删别人的评论"
	// 也回 401，一个手滑点到别人评论删除按钮的用户会被莫名其妙登出。
	// 这是 403 在本项目里最大的实际价值，不是为了教科书上的语义正确。
	ErrForbidden = errors.New("forbidden")
)

// ClassifyHTTPStatus 把错误翻译成合适的 HTTP 状态码
func ClassifyHTTPStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrValidation):
		return http.StatusBadRequest
	case errors.Is(err, gorm.ErrRecordNotFound):
		return http.StatusNotFound
	// ---------- JSON 请求体坏了 → 400，不是 500 ----------
	//
	// 这两类错误只可能来自 c.ShouldBindJSON，含义是**客户端发的东西不是合法 JSON**：
	//
	//	*json.SyntaxError       —— 语法就不对（`{not json`）
	//	*json.UnmarshalTypeError —— 语法对但类型对不上（`"video_id": "abc"`）
	//
	// 不特判的话它们会掉进 default → 500，而那是在说"服务端崩了"，
	// 与事实相反 —— 服务端好好的，是请求本身不合法。后果有三层：
	//
	//  1. 任何监控/告警会把它算成服务端故障，掩盖真正的问题；
	//  2. 排查的人会往服务端日志里找，而那里什么都没有；
	//  3. **对本项目最要命的一条**：手工 curl 调试时字段名写错（比如把
	//     video_id 写成 videoid，或者少一个引号），返回的是 500 ——
	//     看起来完全像是后端坏了，于是整条链路被重新怀疑一遍。
	//     （这条已经踩过：看 gin-ignores-unknown-json-fields 那条记录。）
	//
	// ⚠ 顺序要求：这两个 case 必须排在 default **之前**，而 errors.As 要求
	// 目标是指针的指针 —— 写成 json.SyntaxError 会 panic。
	case asJSONSyntaxError(err) || asJSONTypeError(err):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func asJSONSyntaxError(err error) bool {
	var e *json.SyntaxError
	return errors.As(err, &e)
}

func asJSONTypeError(err error) bool {
	var e *json.UnmarshalTypeError
	return errors.As(err, &e)
}
