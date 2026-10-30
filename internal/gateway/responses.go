// OpenAI Responses 协议模型（M4-W1 骨架，开发文档 §9.5）。
// W1 范围：请求/响应类型定义、入站解析、同协议透传；跨协议翻译与
// 流式事件（response.output_item / response.completed 等）在 W2 补齐
// （translateRequest/translateResponse 遇 Responses 先 fail-closed）。
package gateway

import "strings"

// ResponsesInputItem 是 input 数组的元素。W1 只需建模到「能安全解析」
// 的粒度：role/message 形态（messages 兼容）与 function_call/
// function_call_output 形态（W2 翻译用）；同协议透传走原始字节，
// 不经结构体重序列化（读不懂=未知：未知形态不猜测、不丢弃）。
type ResponsesInputItem struct {
	Type    string `json:"type,omitempty"` // message / function_call / function_call_output
	Role    string `json:"role,omitempty"`
	Content any    `json:"content,omitempty"`

	// function_call（assistant 发起的工具调用）
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// function_call_output（user 侧回传的工具结果）
	Output string `json:"output,omitempty"`
}

// ResponsesTool 是 tools 元素（type 固定 function）。
type ResponsesTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// ResponsesRequest 是 /v1/responses 请求。字段最小集：model/input/
// stream/instructions/max_output_tokens/tools（W1 骨架 + W2 翻译所需）；
// 同协议透传走原始字节，其余字段不在 W1 逐一定义。
type ResponsesRequest struct {
	Model           string          `json:"model"`
	Input           any             `json:"input,omitempty"` // string 或 []ResponsesInputItem
	Stream          bool            `json:"stream,omitempty"`
	Instructions    string          `json:"instructions,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Tools           []ResponsesTool `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
}

// ResponsesOutputItem 是响应 output 数组的元素（非流式形态）。
type ResponsesOutputItem struct {
	Type    string `json:"type"` // message / function_call / reasoning
	ID      string `json:"id,omitempty"`
	Role    string `json:"role,omitempty"`
	Status  string `json:"status,omitempty"`
	Name    string `json:"name,omitempty"`      // function_call
	CallID  string `json:"call_id,omitempty"`   // function_call
	Args    string `json:"arguments,omitempty"` // function_call
	Content any    `json:"content,omitempty"`   // message：[{type,text}]
}

// ResponsesUsage 是 Responses 的用量字段（形状对齐官方：input/output
// 而非 prompt/completion；W4 成本核算对齐时以此为源）。
type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ResponsesResponse 是非流式响应的最小骨架。
type ResponsesResponse struct {
	ID         string                `json:"id"`
	Object     string                `json:"object"` // response
	Model      string                `json:"model,omitempty"`
	Status     string                `json:"status,omitempty"` // completed / incomplete
	Output     []ResponsesOutputItem `json:"output,omitempty"`
	Usage      *ResponsesUsage       `json:"usage,omitempty"`
	Incomplete *struct {
		Reason string `json:"reason,omitempty"`
	} `json:"incomplete_details,omitempty"`
}

// responsesInputText 提取 input 的文本部分（规则引擎 F7/F8 的输入统计）：
// string 直接返回；数组拼接 message 项的文本内容。
func responsesInputText(in any) string {
	switch t := in.(type) {
	case string:
		return t
	case []any:
		var texts []string
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := m["role"].(string); role != "user" && role != "system" && role != "assistant" {
				// function_call / function_call_output 等形态：Arguments/
				// Output 也计入长度估计，但不作为「最后用户消息」
				if s, _ := m["arguments"].(string); s != "" {
					texts = append(texts, s)
				}
				if s, _ := m["output"].(string); s != "" {
					texts = append(texts, s)
				}
				continue
			}
			switch c := m["content"].(type) {
			case string:
				if c != "" {
					texts = append(texts, c)
				}
			case []any:
				for _, part := range c {
					pm, ok := part.(map[string]any)
					if !ok {
						continue
					}
					if tp, _ := pm["type"].(string); tp == "input_text" || tp == "output_text" || tp == "text" {
						if s, _ := pm["text"].(string); s != "" {
							texts = append(texts, s)
						}
					}
				}
			}
		}
		return joinNonEmpty(texts)
	default:
		return ""
	}
}

// responsesLastUser 提取最后一条 user 消息文本（意图分类输入）。
func responsesLastUser(in any) string {
	switch t := in.(type) {
	case string:
		return t
	case []any:
		for i := len(t) - 1; i >= 0; i-- {
			m, ok := t[i].(map[string]any)
			if !ok {
				continue
			}
			if role, _ := m["role"].(string); role != "user" {
				continue
			}
			switch c := m["content"].(type) {
			case string:
				if strings.TrimSpace(c) != "" {
					return c
				}
			case []any:
				var texts []string
				for _, part := range c {
					pm, ok := part.(map[string]any)
					if !ok {
						continue
					}
					if tp, _ := pm["type"].(string); tp == "input_text" || tp == "text" {
						if s, _ := pm["text"].(string); s != "" {
							texts = append(texts, s)
						}
					}
				}
				if s := joinNonEmpty(texts); s != "" {
					return s
				}
			}
		}
		return ""
	default:
		return ""
	}
}

func joinNonEmpty(parts []string) string {
	out := ""
	for i, s := range parts {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out
}
