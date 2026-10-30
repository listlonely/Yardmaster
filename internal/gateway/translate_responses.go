// OpenAI Responses ↔ Chat 的双向翻译（M4-W2，枢纽方案见
// docs/subsystems/protocol.md：Chat 为内部枢纽，Responses 与 Messages
// 经 Chat 中转）。覆盖文本 + 工具定义/调用（function_call ↔
// tool_calls、function_call_output ↔ role:"tool"）；reasoning/多模态
// 块维持忽略（可砍项，与 Chat↔Messages 行为一致）。
package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------- 停止原因映射（对照 Magpie responses.go） ----------

// respStopToChat：Responses status/incomplete → chat finish_reason。
func respStopToChat(status, incompleteReason string, hasToolCall bool) string {
	switch {
	case status == "incomplete" && incompleteReason == "content_filter":
		return "content_filter"
	case status == "incomplete":
		return "length" // max_output_tokens 等截断
	case hasToolCall:
		return "tool_calls"
	default:
		return "stop"
	}
}

// chatFinishToResp：chat finish_reason → Responses status + incomplete 原因。
func chatFinishToResp(finish string) (status, reason string) {
	switch finish {
	case "length":
		return "incomplete", "max_output_tokens"
	case "content_filter":
		return "incomplete", "content_filter"
	default: // stop / tool_calls / ""
		return "completed", ""
	}
}

// ---------- 请求翻译：Responses → Chat（入站归一） ----------

// responsesToChatRequest 把 Responses 请求翻译为 Chat 请求。
// input 为 string 时视作单条 user 消息；数组逐项映射：
// message → 对应角色消息；function_call → assistant 的 tool_calls；
// function_call_output → role:"tool"（tool_call_id=call_id）。
func responsesToChatRequest(rr *ResponsesRequest) *ChatRequest {
	cr := &ChatRequest{Model: rr.Model, Stream: rr.Stream, MaxTokens: rr.MaxOutputTokens}
	if rr.Instructions != "" {
		cr.Messages = append(cr.Messages, ChatMessage{Role: "system", Content: rr.Instructions})
	}
	if s, ok := rr.Input.(string); ok && s != "" {
		cr.Messages = append(cr.Messages, ChatMessage{Role: "user", Content: s})
	} else if items, ok := rr.Input.([]any); ok {
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			tp, _ := m["type"].(string)
			switch tp {
			case "", "message": // 兼容省略 type 的裸 message 项
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				if role == "developer" {
					role = "system"
				}
				cr.Messages = append(cr.Messages, ChatMessage{Role: role, Content: respContentText(m["content"])})
			case "function_call":
				cm := ChatMessage{Role: "assistant"}
				args, _ := m["arguments"].(string)
				cm.ToolCalls = append(cm.ToolCalls, ChatToolCall{
					ID:   respCallID(m),
					Type: "function",
					Function: ChatFunction{
						Name:      m["name"].(string),
						Arguments: args,
					},
				})
				cr.Messages = append(cr.Messages, cm)
			case "function_call_output":
				cr.Messages = append(cr.Messages, ChatMessage{
					Role:       "tool",
					ToolCallID: respCallID(m),
					Content:    respOutputText(m["output"]),
				})
			}
			// reasoning 等未建模形态：忽略（可砍项纪律：不猜测字段）
		}
	}
	for _, t := range rr.Tools {
		if t.Type != "" && t.Type != "function" {
			continue // web_search 等内建工具不翻译（W2 范围外）
		}
		var ct ChatTool
		ct.Type = "function"
		ct.Function.Name = t.Name
		ct.Function.Description = t.Description
		ct.Function.Parameters = t.Parameters
		cr.Tools = append(cr.Tools, ct)
	}
	return cr
}

// respCallID 取 function_call / function_call_output 的关联 ID：call_id
// 优先，缺省回退 id。
func respCallID(m map[string]any) string {
	if s, _ := m["call_id"].(string); s != "" {
		return s
	}
	s, _ := m["id"].(string)
	return s
}

// respContentText 从 message 项的 content（string 或分段数组）提取文本。
func respContentText(c any) string {
	switch t := c.(type) {
	case string:
		return t
	case []any:
		var texts []string
		for _, part := range t {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if tp, _ := pm["type"].(string); tp == "input_text" || tp == "output_text" || tp == "text" || tp == "summary_text" {
				if s, _ := pm["text"].(string); s != "" {
					texts = append(texts, s)
				}
			}
		}
		return strings.Join(texts, "\n")
	default:
		return ""
	}
}

// respOutputText 从 function_call_output 的 output（string 或结构体）提取
// 文本：官方允许 string 或 {content: string} 两种形态。
func respOutputText(o any) string {
	switch t := o.(type) {
	case string:
		return t
	case map[string]any:
		if s, ok := t["content"].(string); ok {
			return s
		}
		b, _ := json.Marshal(t)
		return string(b)
	default:
		return ""
	}
}

// ---------- 请求翻译：Chat → Responses（出站归一） ----------

// chatToResponsesRequest 把 Chat 请求翻译为 Responses 请求。
// system → instructions；tool_calls → function_call 项；role:"tool"
// → function_call_output 项。
func chatToResponsesRequest(cr *ChatRequest) *ResponsesRequest {
	rr := &ResponsesRequest{
		Model:           cr.Model,
		Stream:          cr.Stream,
		MaxOutputTokens: cr.MaxTokens,
		Instructions:    contentText(firstSystem(cr.Messages)),
	}
	var input []any
	addItem := func(item map[string]any) { input = append(input, item) }
	for _, m := range cr.Messages {
		switch m.Role {
		case "system":
			continue // 已并入 instructions
		case "tool":
			addItem(map[string]any{
				"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content,
			})
		case "assistant":
			text := contentText(m.Content)
			if text != "" {
				addItem(map[string]any{"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": text}}})
			}
			for _, tc := range m.ToolCalls {
				addItem(map[string]any{
					"type": "function_call", "call_id": tc.ID, "name": tc.Function.Name,
					"arguments": orDefault(tc.Function.Arguments, "{}"),
				})
			}
		default: // user
			addItem(map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": contentText(m.Content)}}})
		}
	}
	if input != nil {
		rr.Input = input
	}
	for _, t := range cr.Tools {
		rr.Tools = append(rr.Tools, ResponsesTool{
			Type: "function", Name: t.Function.Name,
			Description: t.Function.Description, Parameters: t.Function.Parameters,
		})
	}
	return rr
}

// firstSystem 提取首条 system 消息内容（chat→responses 的 instructions 源）。
func firstSystem(msgs []ChatMessage) any {
	for _, m := range msgs {
		if m.Role == "system" {
			return m.Content
		}
	}
	return nil
}

// ---------- 非流响应翻译 ----------

// responsesResponseToChat 把上游 Responses 非流响应翻译为 Chat 响应。
func responsesResponseToChat(rsp *ResponsesResponse) *ChatChunk {
	var out ChatChunk
	out.ID = rsp.ID
	out.Object = "chat.completion"
	out.Model = rsp.Model
	hasTool := false
	msg := &ChatMessage{Role: "assistant"}
	var texts []string
	for _, item := range rsp.Output {
		switch item.Type {
		case "message":
			texts = append(texts, respContentText(item.Content))
		case "function_call":
			hasTool = true
			msg.ToolCalls = append(msg.ToolCalls, ChatToolCall{
				ID: orDefault(item.CallID, item.ID), Type: "function",
				Function: ChatFunction{Name: item.Name, Arguments: orDefault(item.Args, "{}")},
			})
		}
	}
	msg.Content = strings.Join(texts, "")
	if rsp.Usage != nil {
		out.Usage = &ChatUsage{PromptTokens: rsp.Usage.InputTokens, CompletionTokens: rsp.Usage.OutputTokens}
	}
	incomplete := ""
	if rsp.Incomplete != nil {
		incomplete = rsp.Incomplete.Reason
	}
	choice := ChatChoice{Index: 0, FinishReason: respStopToChat(rsp.Status, incomplete, hasTool)}
	choice.Message = msg
	out.Choices = append(out.Choices, choice)
	return &out
}

// chatResponseToResponses 把上游 Chat 非流响应翻译为 Responses 响应。
// model 为客户端请求的模型名（响应体回显用）。
func chatResponseToResponses(model string, cc *ChatChunk) *ResponsesResponse {
	rsp := &ResponsesResponse{ID: orDefault(cc.ID, "resp_"+model), Object: "response", Model: cc.Model}
	var finish string
	for _, ch := range cc.Choices {
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
		if ch.Message == nil {
			continue
		}
		if s, ok := ch.Message.Content.(string); ok && s != "" {
			rsp.Output = append(rsp.Output, ResponsesOutputItem{
				Type: "message", ID: "msg_0", Role: "assistant", Status: "completed",
				Content: []any{map[string]any{"type": "output_text", "text": s}},
			})
		}
		for _, tc := range ch.Message.ToolCalls {
			rsp.Output = append(rsp.Output, ResponsesOutputItem{
				Type: "function_call", ID: "fc_" + tc.ID, CallID: tc.ID, Status: "completed",
				Name: tc.Function.Name, Args: orDefault(tc.Function.Arguments, "{}"),
			})
		}
	}
	status, reason := chatFinishToResp(finish)
	rsp.Status = status
	if reason != "" {
		rsp.Incomplete = &struct {
			Reason string `json:"reason,omitempty"`
		}{Reason: reason}
	}
	if cc.Usage != nil {
		rsp.Usage = &ResponsesUsage{InputTokens: cc.Usage.PromptTokens, OutputTokens: cc.Usage.CompletionTokens}
	}
	return rsp
}

// ---------- 流式翻译：Chat SSE → Responses 事件 ----------

// chatToRespStream 把上游 chat chunk 流编码为 Responses SSE 事件序列。
// 结构对齐 chatToMsgsStream（Feed/out/Done），事件对照 protocol.md。
type chatToRespStream struct {
	out     []byte
	respID  string
	itemSeq int              // 已开 item 计数（output_index 递增）
	msgOpen bool             // message item 已 added
	textAll strings.Builder  // 累积全文（output_text.done 用）
	fcOpen  map[int]*fcState // chat tool_calls index → 状态
	fcDone  []ResponsesOutputItem
	finish  string
	usage   *ChatUsage
}

type fcState struct {
	itemID    string
	callID    string
	name      string
	args      strings.Builder
	outputIdx int
}

func newChatToRespStream() *chatToRespStream {
	return &chatToRespStream{respID: "resp_" + traceID(time.Now()), fcOpen: map[int]*fcState{}}
}

// Feed 处理一条上游 chat chunk 的 data 体。
func (t *chatToRespStream) Feed(data []byte) error {
	var cc ChatChunk
	if err := json.Unmarshal(data, &cc); err != nil {
		return fmt.Errorf("解析 chat chunk: %w", err)
	}
	if cc.ID != "" {
		t.respID = cc.ID
	}
	if cc.Usage != nil {
		t.usage = cc.Usage
	}
	for _, ch := range cc.Choices {
		if ch.FinishReason != "" {
			t.finish = ch.FinishReason
		}
		if s, ok := ch.Delta.Content.(string); ok && s != "" {
			t.openMessage()
			t.emit("response.output_text.delta", map[string]any{
				"item_id": "msg_0", "output_index": 0, "content_index": 0, "delta": s,
			})
			t.textAll.WriteString(s)
		}
		for _, tc := range ch.Delta.ToolCalls {
			t.openFunctionCall(tc)
		}
	}
	return nil
}

func (t *chatToRespStream) openMessage() {
	if t.msgOpen {
		return
	}
	t.msgOpen = true
	t.emit("response.output_item.added", map[string]any{
		"output_index": 0, "item": map[string]any{
			"type": "message", "id": "msg_0", "role": "assistant", "status": "in_progress", "content": []any{},
		},
	})
	t.emit("response.content_part.added", map[string]any{
		"item_id": "msg_0", "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": ""},
	})
}

func (t *chatToRespStream) openFunctionCall(tc ChatToolCall) {
	st := t.fcOpen[tc.Index]
	if st == nil {
		t.itemSeq++
		st = &fcState{
			itemID: fmt.Sprintf("fc_%d", t.itemSeq), outputIdx: t.itemSeq,
			callID: tc.ID, name: tc.Function.Name,
		}
		t.fcOpen[tc.Index] = st
		t.emit("response.output_item.added", map[string]any{
			"output_index": st.outputIdx, "item": map[string]any{
				"type": "function_call", "id": st.itemID, "call_id": st.callID,
				"name": st.name, "arguments": "",
			},
		})
	}
	if tc.Function.Name != "" && st.name == "" {
		st.name = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		st.args.WriteString(tc.Function.Arguments)
		t.emit("response.function_call_arguments.delta", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIdx, "delta": tc.Function.Arguments,
		})
	}
}

// Done 收尾：闭合各 item 并发 response.completed / incomplete。
func (t *chatToRespStream) Done() []byte {
	if t.msgOpen {
		full := t.textAll.String()
		t.emit("response.output_text.done", map[string]any{
			"item_id": "msg_0", "output_index": 0, "content_index": 0, "text": full,
		})
		t.emit("response.content_part.done", map[string]any{
			"item_id": "msg_0", "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": full},
		})
		t.emit("response.output_item.done", map[string]any{
			"output_index": 0, "item": map[string]any{
				"type": "message", "id": "msg_0", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": full}},
			},
		})
	}
	for i := 0; i <= maxFCIndex(t.fcOpen); i++ {
		st := t.fcOpen[i]
		if st == nil {
			continue
		}
		args := orDefault(st.args.String(), "{}")
		t.emit("response.function_call_arguments.done", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIdx, "arguments": args,
		})
		item := map[string]any{
			"type": "function_call", "id": st.itemID, "call_id": st.callID,
			"name": st.name, "arguments": args, "status": "completed",
		}
		t.emit("response.output_item.done", map[string]any{"output_index": st.outputIdx, "item": item})
		t.fcDone = append(t.fcDone, ResponsesOutputItem{
			Type: "function_call", ID: st.itemID, CallID: st.callID,
			Name: st.name, Args: args, Status: "completed",
		})
	}
	// 最终 response 事件：status 按 finish 映射（对照 chatFinishToResp）。
	resp := map[string]any{
		"id": t.respID, "object": "response", "model": "",
		"status": "completed",
	}
	output := []any{}
	if t.msgOpen {
		output = append(output, map[string]any{
			"type": "message", "id": "msg_0", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": t.textAll.String()}},
		})
	}
	output = append(output, fcItemsAsAny(t.fcDone)...)
	resp["output"] = output
	status, reason := chatFinishToResp(t.finish)
	resp["status"] = status
	if reason != "" {
		resp["incomplete_details"] = map[string]any{"reason": reason}
	}
	if t.usage != nil {
		resp["usage"] = map[string]any{"input_tokens": t.usage.PromptTokens, "output_tokens": t.usage.CompletionTokens}
	}
	evt := "response.completed"
	if status == "incomplete" {
		evt = "response.incomplete"
	}
	t.emit(evt, map[string]any{"type": evt, "response": resp})
	return t.out
}

func maxFCIndex(m map[int]*fcState) int {
	max := -1
	for i := range m {
		if i > max {
			max = i
		}
	}
	return max
}

func fcItemsAsAny(items []ResponsesOutputItem) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"type": "function_call", "id": it.ID, "call_id": it.CallID,
			"name": it.Name, "arguments": it.Args, "status": "completed",
		})
	}
	return out
}

func (t *chatToRespStream) emit(name string, body map[string]any) {
	body["type"] = name
	data, err := json.Marshal(body)
	if err != nil {
		return
	}
	t.out = appendSSE(t.out, name, data)
}

// ---------- 流式翻译：Responses SSE → Chat chunk ----------

// respToChatStream 把上游 Responses 事件流翻译为 chat chunk 流
// （供 Responses 上游 × Chat/Messages 客户端使用；后者经
// chatToMsgsStream 二级串联）。
type respToChatStream struct {
	out     []byte
	model   string
	toolAny bool
	fcArgs  map[string]*strings.Builder // item_id → 参数累积
	fcItems []ResponsesOutputItem
	status  string
	incomp  string
	usage   *ResponsesUsage
}

func newRespToChatStream() *respToChatStream { return &respToChatStream{fcArgs: map[string]*strings.Builder{}} }

// Feed 处理一条上游 Responses 事件（data 体）。
func (t *respToChatStream) Feed(ev sseEvent) error {
	var body struct {
		Type   string `json:"type"`
		Delta  string `json:"delta"`
		Item   *ResponsesOutputItem `json:"item"`
		ItemID string `json:"item_id"`
		Response *struct {
			ID     string          `json:"id"`
			Model  string          `json:"model"`
			Status string          `json:"status"`
			Usage  *ResponsesUsage `json:"usage"`
			Incomplete *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
	}
	if err := json.Unmarshal(ev.Data, &body); err != nil {
		return fmt.Errorf("解析 responses 事件: %w", err)
	}
	switch body.Type {
	case "response.output_text.delta":
		t.emitChat(&ChatChunk{Choices: []ChatChoice{{Delta: ChatMessage{Content: body.Delta}}}})
	case "response.function_call_arguments.delta":
		if body.ItemID != "" {
			b := t.fcArgs[body.ItemID]
			if b == nil {
				b = &strings.Builder{}
				t.fcArgs[body.ItemID] = b
			}
			b.WriteString(body.Delta)
		}
	case "response.output_item.done":
		if body.Item != nil && body.Item.Type == "function_call" {
			// 以 done 项的完整参数为准（delta 累积仅兜底）
			if body.Item.Args == "" {
				body.Item.Args = t.fcArgs[body.Item.ID].String()
			}
			t.fcItems = append(t.fcItems, *body.Item)
			t.toolAny = true
			t.emitToolCall(*body.Item, len(t.fcItems)-1)
		}
	case "response.completed", "response.incomplete", "response.failed":
		if body.Response != nil {
			t.status = orDefault(body.Response.Status, "completed")
			if body.Response.Incomplete != nil {
				t.incomp = body.Response.Incomplete.Reason
			}
			t.usage = body.Response.Usage
			if body.Response.Model != "" {
				t.model = body.Response.Model
			}
		}
		if body.Type == "response.failed" {
			t.status = "failed"
		}
	}
	return nil
}

func (t *respToChatStream) emitToolCall(item ResponsesOutputItem, idx int) {
	cc := &ChatChunk{Choices: []ChatChoice{{
		Delta: ChatMessage{ToolCalls: []ChatToolCall{{
			Index: idx, ID: orDefault(item.CallID, item.ID), Type: "function",
			Function: ChatFunction{Name: item.Name, Arguments: orDefault(item.Args, "{}")},
		}}},
	}}}
	t.emitChat(cc)
}

func (t *respToChatStream) emitChat(cc *ChatChunk) {
	cc.Object = "chat.completion.chunk"
	if cc.Model == "" {
		cc.Model = t.model
	}
	data, err := json.Marshal(cc)
	if err != nil {
		return
	}
	t.out = appendSSE(t.out, "", data)
}

// Done 输出终止 chunk（finish_reason + usage）与 [DONE]。
func (t *respToChatStream) Done() []byte {
	status := t.status
	if status == "" {
		status = "completed"
	}
	finish := respStopToChat(status, t.incomp, t.toolAny || len(t.fcItems) > 0)
	cc := &ChatChunk{Choices: []ChatChoice{{Delta: ChatMessage{}, FinishReason: finish}}}
	if t.usage != nil {
		cc.Usage = &ChatUsage{PromptTokens: t.usage.InputTokens, CompletionTokens: t.usage.OutputTokens}
	}
	data, _ := json.Marshal(cc)
	t.out = appendSSE(t.out, "", data)
	t.out = append(t.out, []byte("data: [DONE]\n\n")...)
	return t.out
}
