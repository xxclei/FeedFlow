package apierror

import (
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
	default:
		return http.StatusInternalServerError
	}
}
