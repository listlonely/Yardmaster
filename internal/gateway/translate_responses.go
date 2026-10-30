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

// ---------- 流式翻译：事件 → 客户端 Responses 事件（M5-W1 encoder） ----------

// respEncoder 把 gwEvent 编码为 Responses SSE 事件序列（承接 M4
// chatToRespStream 的 item 状态机与 response.completed 收尾）。
type respEncoder struct {
	out     *sseOut
	respID  string
	itemSeq int              // 已开 item 计数（output_index 递增）
	msgOpen bool             // message item 已 added
	textAll strings.Builder  // 累积全文（output_text.done 用）
	fcOpen  map[int]*fcState // 事件工具顺序号 → 状态
	fcDone  []ResponsesOutputItem
	finish  string
	usage   *gwUsage
}

type fcState struct {
	itemID    string
	callID    string
	name      string
	args      strings.Builder
	outputIdx int
}

func newRespEncoder(out *sseOut) *respEncoder {
	return &respEncoder{out: out, respID: "resp_" + traceID(time.Now()), fcOpen: map[int]*fcState{}}
}

func (t *respEncoder) emit(name string, body map[string]any) {
	body["type"] = name
	t.out.emit(name, body)
}

func (t *respEncoder) Encode(ev gwEvent) {
	switch ev.kind {
	case evStart:
		if ev.msgID != "" {
			t.respID = ev.msgID
		}
	case evText:
		t.openMessage()
		t.emit("response.output_text.delta", map[string]any{
			"item_id": "msg_0", "output_index": 0, "content_index": 0, "delta": ev.text,
		})
		t.textAll.WriteString(ev.text)
	case evToolStart:
		t.toolStart(ev.idx, ev.id, ev.name)
	case evToolArgs:
		if st := t.fcOpen[ev.idx]; st != nil && ev.text != "" {
			st.args.WriteString(ev.text)
			t.emit("response.function_call_arguments.delta", map[string]any{
				"item_id": st.itemID, "output_index": st.outputIdx, "delta": ev.text,
			})
		}
	case evStop:
		if ev.stop != "" {
			t.finish = ev.stop
		}
	case evUsage:
		u := ev.usage
		t.usage = &u
	}
}

func (t *respEncoder) openMessage() {
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

// toolStart：新顺序号开 function_call item；已知顺序号名字后到则补记。
func (t *respEncoder) toolStart(idx int, id, name string) {
	st := t.fcOpen[idx]
	if st == nil {
		t.itemSeq++
		st = &fcState{
			itemID: fmt.Sprintf("fc_%d", t.itemSeq), outputIdx: t.itemSeq,
			callID: id, name: name,
		}
		t.fcOpen[idx] = st
		t.emit("response.output_item.added", map[string]any{
			"output_index": st.outputIdx, "item": map[string]any{
				"type": "function_call", "id": st.itemID, "call_id": st.callID,
				"name": st.name, "arguments": "",
			},
		})
	}
	if name != "" && st.name == "" {
		st.name = name
	}
}

// Finish 收尾：闭合各 item 并发 response.completed / incomplete。
func (t *respEncoder) Finish() {
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
		resp["usage"] = map[string]any{"input_tokens": t.usage.prompt, "output_tokens": t.usage.completion}
	}
	evt := "response.completed"
	if status == "incomplete" {
		evt = "response.incomplete"
	}
	t.emit(evt, map[string]any{"type": evt, "response": resp})
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

// ---------- 流式翻译：上游 Responses SSE → 事件（M5-W1 decoder） ----------

// respDecoder 解析上游 Responses 事件（承接 M4 respToChatStream：文本
// delta 直通、function_call 以 output_item.done 完整参数为准、终态事件
// 提取 status/usage）。
type respDecoder struct {
	toolAny bool
	fcArgs  map[string]*strings.Builder // item_id → 参数累积（done 缺参时兜底）
	fcItems []ResponsesOutputItem
	status  string
	incomp  string
	usage   *ResponsesUsage
}

func newRespDecoder() *respDecoder { return &respDecoder{fcArgs: map[string]*strings.Builder{}} }

// Feed 处理一条上游 Responses 事件（data 体）。
func (d *respDecoder) Feed(ev sseEvent) ([]gwEvent, error) {
	var body struct {
		Type     string               `json:"type"`
		Delta    string               `json:"delta"`
		Item     *ResponsesOutputItem `json:"item"`
		ItemID   string               `json:"item_id"`
		Response *struct {
			ID         string          `json:"id"`
			Model      string          `json:"model"`
			Status     string          `json:"status"`
			Usage      *ResponsesUsage `json:"usage"`
			Incomplete *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
	}
	if err := json.Unmarshal(ev.Data, &body); err != nil {
		return nil, fmt.Errorf("解析 responses 事件: %w", err)
	}
	switch body.Type {
	case "response.output_text.delta":
		if body.Delta != "" {
			return []gwEvent{{kind: evText, text: body.Delta}}, nil
		}
	case "response.function_call_arguments.delta":
		if body.ItemID != "" {
			b := d.fcArgs[body.ItemID]
			if b == nil {
				b = &strings.Builder{}
				d.fcArgs[body.ItemID] = b
			}
			b.WriteString(body.Delta)
		}
	case "response.output_item.done":
		if body.Item != nil && body.Item.Type == "function_call" {
			// 以 done 项的完整参数为准（delta 累积仅兜底）
			if body.Item.Args == "" {
				body.Item.Args = d.fcArgs[body.Item.ID].String()
			}
			d.fcItems = append(d.fcItems, *body.Item)
			d.toolAny = true
			idx := len(d.fcItems) - 1
			item := d.fcItems[idx]
			return []gwEvent{
				{kind: evToolStart, idx: idx, id: orDefault(item.CallID, item.ID), name: item.Name},
				{kind: evToolArgs, idx: idx, text: orDefault(item.Args, "{}")},
				{kind: evToolDone, idx: idx},
			}, nil
		}
	case "response.completed", "response.incomplete", "response.failed":
		if body.Response != nil {
			d.status = orDefault(body.Response.Status, "completed")
			if body.Response.Incomplete != nil {
				d.incomp = body.Response.Incomplete.Reason
			}
			d.usage = body.Response.Usage
		}
		if body.Type == "response.failed" {
			d.status = "failed"
		}
	}
	return nil, nil
}

// Done 输出终止事件（finish 映射 + usage 快照）。
func (d *respDecoder) Done() []gwEvent {
	status := d.status
	if status == "" {
		status = "completed"
	}
	finish := respStopToChat(status, d.incomp, d.toolAny || len(d.fcItems) > 0)
	out := []gwEvent{{kind: evStop, stop: finish}}
	if d.usage != nil {
		out = append(out, gwEvent{kind: evUsage, usage: gwUsage{
			prompt: d.usage.InputTokens, completion: d.usage.OutputTokens,
		}})
	}
	return out
}
