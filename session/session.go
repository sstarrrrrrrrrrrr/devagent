package session

import "github.com/openai/openai-go/v3/responses"

// Session 保存一次对话的完整历史。
type Session struct {
	History []responses.ResponseInputItemUnionParam
}

// NewSession 创建一个空会话。
func NewSession() *Session {
	return &Session{}
}
