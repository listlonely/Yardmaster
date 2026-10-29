// 双协议类型（OpenAI Chat Completions / Anthropic Messages）与 SSE 扫描。
// 类型只覆盖 W2 闭环所需的字段（文本 + 工具定义/调用）；未覆盖字段
// 不透传，W5 补齐（设计文档：翻译矩阵 MVP 范围）。
package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ClientProto 标识调用方使用的 API 协议。
type ClientProto string

const (
	ProtoChat     ClientProto = "openai-chat"
	ProtoMessages ClientProto = "anthropic-messages"
)

// ---------- OpenAI Chat ----------

// ChatFunction 是 tool_call 的函数信息（具名类型：避免在嵌套复合
// 字面量中书写匿名结构体）。
type ChatFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ChatToolCall 对应 chat 的 tool_calls 元素。
type ChatToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"` // "function"
	Function ChatFunction `json:"function,omitempty"`
}

// ChatTool 对应 chat 的 tools 元素。
type ChatTool struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Parameters  any    `json:"parameters,omitempty"`
	} `json:"function"`
}

// ChatMessage 是 chat 的一条消息；Content 可为字符串或分段数组。
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// ChatRequest 是 /v1/chat/completions 请求。
type ChatRequest struct {
	Model       string      `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool        `json:"stream,omitempty"`
	MaxTokens   int         `json:"max_tokens,omitempty"`
	Temperature *float64    `json:"temperature,omitempty"`
	TopP        *float64    `json:"top_p,omitempty"`
	Stop        any         `json:"stop,omitempty"`
	Tools       []ChatTool  `json:"tools,omitempty"`
}

// ChatChoice 是流式/非流响应的通用选择项。
type ChatChoice struct {
	Index        int          `json:"index"`
	Delta        ChatMessage  `json:"delta,omitempty"`
	Message      *ChatMessage `json:"message,omitempty"`
	FinishReason string       `json:"finish_reason"`
}

// ChatChunk 是流式响应的单个 SSE 数据体（非流响应复用 Choice 结构）。
type ChatChunk struct {
	ID      string       `json:"id,omitempty"`
	Object  string       `json:"object,omitempty"`
	Model   string       `json:"model,omitempty"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage,omitempty"`
}

// ChatUsage 是 token 用量。
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// ---------- Anthropic Messages ----------

// Block 是 messages 协议的内容块。
type Block struct {
	Type      string `json:"type"` // text / tool_use / tool_result
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`     // tool_use
	Name      string `json:"name,omitempty"`   // tool_use
	Input     any    `json:"input,omitempty"`  // tool_use
	ToolUseID string `json:"tool_use_id,omitempty"` // tool_result
	Content   any    `json:"content,omitempty"`     // tool_result
}

// AnthropicTool 对应 messages 的 tools 元素。
type AnthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
}

// MessagesMessage 是 messages 的一条消息；Content 可为字符串或块数组。
type MessagesMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// MessagesRequest 是 /v1/messages 请求。
type MessagesRequest struct {
	Model     string            `json:"model"`
	Messages  []MessagesMessage `json:"messages"`
	System    any               `json:"system,omitempty"`
	Stream    bool              `json:"stream,omitempty"`
	MaxTokens int               `json:"max_tokens,omitempty"`
	Tools     []AnthropicTool   `json:"tools,omitempty"`
}

// MessagesResponse 是非流式响应。
type MessagesResponse struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Role       string  `json:"role"`
	Model      string  `json:"model,omitempty"`
	Content    []Block `json:"content"`
	StopReason string  `json:"stop_reason,omitempty"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// msgEvent 是 anthropic SSE 事件体的通用形态。
type msgEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type        string `json:"type,omitempty"`
		Text        string `json:"text,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
		StopReason  string `json:"stop_reason,omitempty"`
	} `json:"delta,omitempty"`
	ContentBlock *Block `json:"content_block,omitempty"`
	Message      *struct {
		Model string `json:"model,omitempty"`
	} `json:"message,omitempty"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
}

// ---------- 内容助手 ----------

// contentText 从 string 或 []Block 形态的内容中提取纯文本（拼接文本块）。
func contentText(c any) string {
	switch t := c.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if tp, _ := m["type"].(string); tp == "text" {
					if s, _ := m["text"].(string); s != "" {
						if b.Len() > 0 {
							b.WriteString("\n")
						}
						b.WriteString(s)
					}
				}
			}
		}
		return b.String()
	case []Block:
		var b strings.Builder
		for _, blk := range t {
			if blk.Type == "text" && blk.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// toolResultsOf 从用户消息的块数组中分离 tool_result 块与剩余文本。
func splitToolResults(c any) (results []Block, text string) {
	blocks, ok := c.([]any)
	if !ok {
		return nil, contentText(c)
	}
	var texts []string
	for _, item := range blocks {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		tp, _ := m["type"].(string)
		if tp == "tool_result" {
			raw, _ := json.Marshal(m)
			var blk Block
			if json.Unmarshal(raw, &blk) == nil {
				results = append(results, blk)
				continue
			}
		}
		if tp == "text" {
			if s, _ := m["text"].(string); s != "" {
				texts = append(texts, s)
			}
		}
	}
	return results, strings.Join(texts, "\n")
}

// toolUseBlocksOf 从 assistant 内容中提取 tool_use 块（W5 前 thinking 等
// 块类型被忽略并在决策日志记录）。
func toolUseBlocks(c any) []Block {
	var out []Block
	switch t := c.(type) {
	case []any:
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if tp, _ := m["type"].(string); tp == "tool_use" {
					raw, _ := json.Marshal(m)
					var blk Block
					if json.Unmarshal(raw, &blk) == nil {
						out = append(out, blk)
					}
				}
			}
		}
	case []Block:
		for _, blk := range t {
			if blk.Type == "tool_use" {
				out = append(out, blk)
			}
		}
	}
	return out
}

// ---------- SSE ----------

// sseEvent 是一条 SSE 事件。
type sseEvent struct {
	Name string // event: 行的值；openai 上游为空
	Data []byte // data: 行的值（多行以 \n 连接）
}

// scanSSE 从 br 读取一条完整 SSE 事件（以空行结束）。返回 io.EOF 表示
// 流正常结束。注释行（":" 开头）被跳过。
func scanSSE(br *bufio.Reader) (sseEvent, error) {
	var ev sseEvent
	var datas []string
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if err != nil {
			if err == io.EOF {
				if trimmed != "" {
					collectSSELine(&ev, &datas, trimmed)
				}
				if len(datas) > 0 {
					finalizeSSE(&ev, datas)
					return ev, nil
				}
				return sseEvent{}, io.EOF
			}
			return sseEvent{}, err
		}
		if trimmed == "" {
			if ev.Data != nil || len(datas) > 0 {
				finalizeSSE(&ev, datas)
				return ev, nil
			}
			continue // 事件间空行
		}
		if strings.HasPrefix(trimmed, ":") {
			continue // 注释/心跳
		}
		collectSSELine(&ev, &datas, trimmed)
	}
}

func collectSSELine(ev *sseEvent, datas *[]string, line string) {
	if after, ok := strings.CutPrefix(line, "data:"); ok {
		*datas = append(*datas, strings.TrimSpace(after))
		return
	}
	if after, ok := strings.CutPrefix(line, "event:"); ok {
		ev.Name = strings.TrimSpace(after)
		return
	}
	// 其他字段（id:/retry:）W2 不需要
}

func finalizeSSE(ev *sseEvent, datas []string) {
	if len(datas) == 1 {
		ev.Data = []byte(datas[0])
		return
	}
	ev.Data = []byte(strings.Join(datas, "\n"))
}

// appendSSE 把一条事件按 SSE 格式追加到 out（name 为空则只写 data 行）。
func appendSSE(out []byte, name string, data []byte) []byte {
	if name != "" {
		out = append(out, []byte("event: "+name+"\n")...)
	}
	out = append(out, []byte("data: ")...)
	out = append(out, data...)
	return append(out, []byte("\n\n")...)
}

// writeErr 按调用方协议的错误格式输出（W2：结构对齐，字段最小集）。
func writeErr(p ClientProto, status int, msg string) (int, []byte) {
	var body []byte
	if p == ProtoMessages {
		body, _ = json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{"type": "api_error", "message": msg},
		})
	} else {
		body, _ = json.Marshal(map[string]any{
			"error": map[string]any{"message": msg, "type": "agenthub_error"},
		})
	}
	return status, body
}
