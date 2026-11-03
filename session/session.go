package session

import "github.com/openai/openai-go/v3/responses"

// 保存一次对话的完整历史。
type Session struct {
	History []responses.ResponseInputItemUnionParam
}

// 创建一个空会话。
func NewSession() *Session {
	return &Session{History: make([]responses.ResponseInputItemUnionParam, 0)}
}
