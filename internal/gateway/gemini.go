// Google Gemini 协议（M4-W3）：模型定义与 Chat 枢纽双向翻译。
// 形状以官方公开文档为基准、对照 Magpie gemini.go 核对：入站
// /v1beta/models/{model}:generateContent|:streamGenerateContent（alt=sse），
// 上游同形（模型名在上游 URL）；认证 x-goog-api-key（Google 拒绝
// Bearer key）。thought/多模态 part 维持忽略（可砍项）。
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// ---------- Gemini 模型 ----------

// GeminiPart 是 contents[].parts 的元素。文本/工具调用/工具结果三形态；
// 图片（inlineData/fileData）M10-W2 起建模（IR v2）；thought（思考文本）
// M10-W3 起建模（响应方向 → evThinking → 各协议思考块）；其余形态忽略。
type GeminiPart struct {
	Text             string        `json:"text,omitempty"`
	Thought          bool          `json:"thought,omitempty"` // 思考文本标记（M10-W3）
	FunctionCall     *GeminiFnCall `json:"functionCall,omitempty"`
	FunctionResponse *GeminiFnResp `json:"functionResponse,omitempty"`
	InlineData       *GeminiInline `json:"inlineData,omitempty"`
	FileData         *GeminiFile   `json:"fileData,omitempty"`
}

// GeminiInline 是内联二进制（图片等）: MIME + base64。
type GeminiInline struct {
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

// GeminiFile 是 fileData：文件 URI 引用（出站不生成——需先上传，URL 图
// 到 Gemini 上游丢弃+留痕，Magpie 同决策）。
type GeminiFile struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri,omitempty"`
}

type GeminiFnCall struct {
	Name string `json:"name"`
	Args any    `json:"args,omitempty"`
}

type GeminiFnResp struct {
	Name     string `json:"name"`
	Response any    `json:"response,omitempty"`
}

// GeminiContent 是 contents 的元素。
type GeminiContent struct {
	Role  string       `json:"role,omitempty"` // user / model
	Parts []GeminiPart `json:"parts"`
}

// GeminiToolDecl 是 tools[].functionDeclarations 的元素。
type GeminiToolDecl struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type GeminiTool struct {
	FunctionDeclarations []GeminiToolDecl `json:"functionDeclarations,omitempty"`
}

// GeminiRequest 是 generateContent 请求（model/stream 在 URL，不在 body；
// Model 为网关侧注入的非序列化字段，供翻译到其他协议时使用）。
type GeminiRequest struct {
	Contents          []GeminiContent `json:"contents,omitempty"`
	SystemInstruction *GeminiContent  `json:"systemInstruction,omitempty"`
	Tools             []GeminiTool    `json:"tools,omitempty"`
	GenerationConfig  *GeminiGenCfg   `json:"generationConfig,omitempty"`
	Model             string          `json:"-"`
	Ignored           []string        `json:"-"` // 跨协议翻译忽略块留痕（D6）
}

type GeminiGenCfg struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

// GeminiResponse 是非流响应与流式单事件的共同形态。
type GeminiResponse struct {
	Candidates []GeminiCandidate `json:"candidates,omitempty"`
	UsageMeta  *GeminiUsage      `json:"usageMetadata,omitempty"`
}

type GeminiCandidate struct {
	Content      GeminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
}

type GeminiUsage struct {
	PromptTokenCount        int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount    int `json:"candidatesTokenCount,omitempty"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"` // 含于 PromptTokenCount
	ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`      // 思考 token（M10-W3 D4；含于 CandidatesTokenCount）
}

// geminiUpstreamPath 拼上游 URL（模型名在路径里，Magpie GeminiPath 同形）。
func geminiUpstreamPath(model string, stream bool) string {
	if stream {
		return "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
	}
	return "/v1beta/models/" + model + ":generateContent"
}

// ---------- 入站路由解析 ----------

// geminiInboundKey 是 Gemini 入站信息（model/stream 在 URL）的 context 键。
type geminiInboundKey struct{}

type geminiInbound struct {
	model  string
	stream bool
}

// handleGeminiInbound 解析 /v1beta/models/{model}:{method} 并转入推理
// 流水线。通配段后随 ":method" 后缀，ServeMux 模式不支持（bad wildcard
// segment），故挂在 /v1beta/ 前缀下手动解析（escaped path，%2F 模型名
// 先切后解）。countTokens 为 M4 可砍项，未支持。
func (s *Server) handleGeminiInbound(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/v1beta/models/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	idx := strings.LastIndex(rest, ":")
	if idx <= 0 {
		http.NotFound(w, r)
		return
	}
	model, err := url.PathUnescape(rest[:idx])
	if err != nil || model == "" {
		http.NotFound(w, r)
		return
	}
	var stream bool
	switch rest[idx+1:] {
	case "generateContent":
	case "streamGenerateContent":
		stream = true
	default:
		http.NotFound(w, r)
		return
	}
	gi := geminiInbound{model: model, stream: stream}
	s.handleInference(ProtoGemini)(w, r.WithContext(context.WithValue(r.Context(), geminiInboundKey{}, gi)))
}

// ---------- 停止原因映射（protocol.md 对照表） ----------

func geminiStopToChat(reason string, hasTool bool) string {
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	case "STOP", "OTHER", "MALFORMED_FUNCTION_CALL", "":
		if hasTool {
			return "tool_calls"
		}
		return "stop"
	default:
		if hasTool {
			return "tool_calls"
		}
		return "stop"
	}
}

func chatFinishToGemini(finish string) string {
	switch finish {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	case "tool_calls":
		return "STOP" // Gemini 以 functionCall part 表达，finishReason 仍 STOP
	default:
		return "STOP"
	}
}

// ---------- 请求翻译：Gemini → Chat ----------

// geminiToChatRequest 把 Gemini 请求翻译为 Chat 请求。model 由入站
// handler 从 URL 提取后注入（Gemini body 无 model 字段）。
// IR v2（M10-W2）：inlineData/fileData 图片 → IRPart → chat 分段；
// role=model 的图片（生成图历史）chat assistant 不携带，计入 Ignored。
func geminiToChatRequest(gr *GeminiRequest, model string, stream bool) *ChatRequest {
	cr := &ChatRequest{Model: model, Stream: stream, Ignored: gr.Ignored}
	if gr.SystemInstruction != nil {
		parts := geminiPartsOf(gr.SystemInstruction.Parts)
		if c := irToChatContent(parts); c != nil {
			if s, ok := c.(string); ok {
				cr.Messages = append(cr.Messages, ChatMessage{Role: "system", Content: s})
			} else {
				irNote(&cr.Ignored, "image(system，chat system 仅文本)")
			}
		}
	}
	for _, c := range gr.Contents {
		role := "user"
		if c.Role == "model" {
			role = "assistant"
		}
		parts := geminiPartsOf(c.Parts)
		var toolCalls []ChatToolCall
		fcIdx := 0
		for _, p := range c.Parts {
			if p.FunctionCall != nil {
				args, _ := json.Marshal(p.FunctionCall.Args)
				toolCalls = append(toolCalls, ChatToolCall{
					// Gemini 无 call id：以 name 为关联键（functionResponse 同名回传）
					ID: "gemini-" + p.FunctionCall.Name, Index: fcIdx, Type: "function",
					Function: ChatFunction{Name: p.FunctionCall.Name, Arguments: orDefault(string(args), "{}")},
				})
				fcIdx++
			}
		}
		if role == "user" {
			for _, p := range c.Parts {
				if p.FunctionResponse != nil {
					resp := p.FunctionResponse.Response
					if resp == nil {
						resp = map[string]any{}
					}
					b, _ := json.Marshal(resp)
					cr.Messages = append(cr.Messages, ChatMessage{
						Role: "tool", ToolCallID: "gemini-" + p.FunctionResponse.Name, Content: string(b),
					})
				}
			}
			// 旧行为保形：纯空白文本不产生消息；有图片则即使无文本也产生
			hasImage := false
			for _, p := range parts {
				if p.Kind != "text" {
					hasImage = true
					break
				}
			}
			if hasImage || strings.TrimSpace(geminiPartsText(c.Parts)) != "" {
				if c := irToChatContent(parts); c != nil {
					cr.Messages = append(cr.Messages, ChatMessage{Role: role, Content: c})
				}
			}
			continue
		}
		// assistant（role=model）：文本 + 工具调用；图片历史不携带 → 留痕
		for _, p := range parts {
			if p.Kind != "text" {
				irNote(&cr.Ignored, "image(model 历史，chat assistant 不携带)")
				break
			}
		}
		msg := ChatMessage{Role: role, Content: geminiPartsText(c.Parts)}
		msg.ToolCalls = toolCalls
		cr.Messages = append(cr.Messages, msg)
	}
	for _, t := range gr.Tools {
		for _, d := range t.FunctionDeclarations {
			ct := ChatTool{Type: "function"}
			ct.Function.Name = d.Name
			ct.Function.Description = d.Description
			ct.Function.Parameters = d.Parameters
			cr.Tools = append(cr.Tools, ct)
		}
	}
	if gr.GenerationConfig != nil {
		cr.MaxTokens = gr.GenerationConfig.MaxOutputTokens
		cr.Temperature = gr.GenerationConfig.Temperature
		cr.TopP = gr.GenerationConfig.TopP
	}
	return cr
}

func geminiPartsText(parts []GeminiPart) string {
	var texts []string
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// ---------- 请求翻译：Chat → Gemini ----------

// chatToGeminiRequest 把 Chat 请求翻译为 Gemini 请求。
// IR v2（M10-W2）：chat 分段图片 → inlineData（URL 图丢弃+留痕：
// fileData 需先上传文件，Magpie 同决策）；system 仅文本。
func chatToGeminiRequest(cr *ChatRequest) *GeminiRequest {
	gr := &GeminiRequest{Ignored: cr.Ignored}
	if sys := firstSystem(cr.Messages); sys != nil {
		var ig []string
		parts := chatContentParts(sys, &ig)
		texts := make([]IRPart, 0, len(parts))
		for _, p := range parts {
			if p.Kind == "text" {
				texts = append(texts, p)
			} else {
				irNote(&ig, "image(system，gemini systemInstruction 仅文本)")
			}
		}
		if c := irToChatContent(texts); c != nil {
			if s, ok := c.(string); ok && s != "" {
				gr.SystemInstruction = &GeminiContent{Parts: []GeminiPart{{Text: s}}}
			}
		}
		gr.Ignored = append(gr.Ignored, ig...)
	}
	for _, m := range cr.Messages {
		switch m.Role {
		case "system":
			continue
		case "tool":
			var resp any
			if s, ok := m.Content.(string); ok && s != "" {
				var v any
				if json.Unmarshal([]byte(s), &v) == nil {
					resp = v
				} else {
					resp = map[string]any{"output": s}
				}
			}
			name := strings.TrimPrefix(m.ToolCallID, "gemini-")
			gr.Contents = append(gr.Contents, GeminiContent{Role: "user", Parts: []GeminiPart{{
				FunctionResponse: &GeminiFnResp{Name: name, Response: resp},
			}}})
		case "assistant":
			var ig []string
			parts := chatContentParts(m.Content, &ig)
			var gParts []GeminiPart
			for _, p := range parts {
				if p.Kind == "text" {
					if p.Text != "" {
						gParts = append(gParts, GeminiPart{Text: p.Text})
					}
				} else {
					irNote(&ig, "image(assistant，model 历史仅文本)")
				}
			}
			for _, tc := range m.ToolCalls {
				args := tc.Function.Arguments
				var v any
				if args == "" || json.Unmarshal([]byte(args), &v) != nil {
					v = map[string]any{}
				}
				gParts = append(gParts, GeminiPart{FunctionCall: &GeminiFnCall{Name: tc.Function.Name, Args: v}})
			}
			if len(gParts) > 0 {
				gr.Contents = append(gr.Contents, GeminiContent{Role: "model", Parts: gParts})
			}
			gr.Ignored = append(gr.Ignored, ig...)
		default:
			var ig []string
			parts := chatContentParts(m.Content, &ig)
			gParts := irToGeminiParts(parts, &ig)
			if len(gParts) > 0 {
				gr.Contents = append(gr.Contents, GeminiContent{Role: "user", Parts: gParts})
			}
			gr.Ignored = append(gr.Ignored, ig...)
		}
	}
	if len(cr.Tools) > 0 {
		var decls []GeminiToolDecl
		for _, t := range cr.Tools {
			decls = append(decls, GeminiToolDecl{
				Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters,
			})
		}
		gr.Tools = []GeminiTool{{FunctionDeclarations: decls}}
	}
	if cr.MaxTokens > 0 || cr.Temperature != nil || cr.TopP != nil {
		gr.GenerationConfig = &GeminiGenCfg{
			MaxOutputTokens: cr.MaxTokens, Temperature: cr.Temperature, TopP: cr.TopP,
		}
	}
	return gr
}

// ---------- 非流响应翻译 ----------

// geminiResponseToChat 把上游 Gemini 响应翻译为 Chat 响应。
func geminiResponseToChat(gr *GeminiResponse) *ChatChunk {
	var out ChatChunk
	out.Object = "chat.completion"
	msg := &ChatMessage{Role: "assistant"}
	var texts []string
	var thoughts []string
	hasTool := false
	fcIdx := 0
	for _, cand := range gr.Candidates {
		for _, p := range cand.Content.Parts {
			if p.Text != "" {
				if p.Thought { // M10-W3：思考文本入 hub Reasoning（→ 各客户端思考块）
					thoughts = append(thoughts, p.Text)
					continue
				}
				texts = append(texts, p.Text)
			}
			if p.FunctionCall != nil {
				hasTool = true
				args, _ := json.Marshal(p.FunctionCall.Args)
				msg.ToolCalls = append(msg.ToolCalls, ChatToolCall{
					Index: fcIdx, ID: "gemini-" + p.FunctionCall.Name, Type: "function",
					Function: ChatFunction{Name: p.FunctionCall.Name, Arguments: orDefault(string(args), "{}")},
				})
				fcIdx++
			}
		}
	}
	msg.Content = strings.Join(texts, "")
	msg.Reasoning = strings.Join(thoughts, "")
	if gr.UsageMeta != nil {
		// 入站 gemini 细分折叠为 chat「prompt 含命中」（cachedContentTokenCount
		// 含于 promptTokenCount——M6-W1 校准）。
		out.Usage = &ChatUsage{
			PromptTokens:     gr.UsageMeta.PromptTokenCount,
			CompletionTokens: gr.UsageMeta.CandidatesTokenCount,
		}
		if gr.UsageMeta.CachedContentTokenCount > 0 {
			out.Usage.PromptTokensDetails = &struct {
				CachedTokens int `json:"cached_tokens"`
			}{CachedTokens: gr.UsageMeta.CachedContentTokenCount}
		}
	}
	finish := "stop"
	for _, cand := range gr.Candidates {
		if cand.FinishReason != "" {
			finish = geminiStopToChat(cand.FinishReason, hasTool)
			break
		}
	}
	choice := ChatChoice{Index: 0, FinishReason: finish}
	choice.Message = msg
	out.Choices = append(out.Choices, choice)
	return &out
}

// chatResponseToGemini 把上游 Chat 响应翻译为 Gemini 响应。
func chatResponseToGemini(cc *ChatChunk) *GeminiResponse {
	gr := &GeminiResponse{}
	var finish string
	for _, ch := range cc.Choices {
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
		if ch.Message == nil {
			continue
		}
		var parts []GeminiPart
		if ch.Message.Reasoning != "" { // M10-W3：思考 → thought part（前置于正文）
			parts = append(parts, GeminiPart{Text: ch.Message.Reasoning, Thought: true})
		}
		if s, ok := ch.Message.Content.(string); ok && s != "" {
			parts = append(parts, GeminiPart{Text: s})
		}
		for _, tc := range ch.Message.ToolCalls {
			var v any
			if tc.Function.Arguments == "" || json.Unmarshal([]byte(tc.Function.Arguments), &v) != nil {
				v = map[string]any{}
			}
			parts = append(parts, GeminiPart{FunctionCall: &GeminiFnCall{Name: tc.Function.Name, Args: v}})
		}
		if len(parts) > 0 {
			gr.Candidates = append(gr.Candidates, GeminiCandidate{Content: GeminiContent{Role: "model", Parts: parts}})
		}
	}
	if finish != "" {
		if len(gr.Candidates) == 0 {
			gr.Candidates = append(gr.Candidates, GeminiCandidate{})
		}
		gr.Candidates[0].FinishReason = chatFinishToGemini(finish)
	}
	if cc.Usage != nil {
		// gemini 出口：cachedContentTokenCount 含于 promptTokenCount（M6-W1 校准）；
		// thoughtsTokenCount 入 usageMetadata（M10-W3 D4，有则带）。
		cached := 0
		if cc.Usage.PromptTokensDetails != nil {
			cached = cc.Usage.PromptTokensDetails.CachedTokens
		}
		gr.UsageMeta = &GeminiUsage{
			PromptTokenCount:        cc.Usage.PromptTokens,
			CandidatesTokenCount:    cc.Usage.CompletionTokens,
			CachedContentTokenCount: cached,
		}
		if cc.Usage.CompletionTokensDetails != nil {
			gr.UsageMeta.ThoughtsTokenCount = cc.Usage.CompletionTokensDetails.ReasoningTokens
		}
	}
	return gr
}

// ---------- 流式翻译：事件 → 客户端 Gemini SSE（M5-W1 encoder） ----------

// geminiEncoder 把 gwEvent 编码为 Gemini 流式事件（每事件一个 data JSON，
// 无 [DONE]——Google SDK 兼容，Magpie 同款；承接 M4 chatToGeminiStream）。
// 工具调用在参数到位时输出 functionCall part（名字先到则缓存）。
type geminiEncoder struct {
	out       *sseOut
	parts     []GeminiPart
	finish    string
	usage     *gwUsage
	toolNames map[int]string // 顺序号 → 工具名（参数到位时取用）
	toolEmits map[int]bool   // 顺序号 → 是否已输出过 part
}

func newGeminiEncoder(out *sseOut) *geminiEncoder {
	return &geminiEncoder{out: out, toolNames: map[int]string{}, toolEmits: map[int]bool{}}
}

func (t *geminiEncoder) emitParts(parts []GeminiPart) {
	chunk := GeminiResponse{Candidates: []GeminiCandidate{{Content: GeminiContent{Role: "model", Parts: parts}}}}
	t.parts = append(t.parts, parts...)
	t.out.emit("", chunk)
}

// fnPart 输出一个 functionCall part 并累积（args 解析失败/为空回退 {}）。
func (t *geminiEncoder) fnPart(idx int, args string) {
	var v any
	if args == "" || json.Unmarshal([]byte(args), &v) != nil {
		v = map[string]any{}
	}
	t.toolEmits[idx] = true
	t.emitParts([]GeminiPart{{FunctionCall: &GeminiFnCall{Name: t.toolNames[idx], Args: v}}})
}

func (t *geminiEncoder) Encode(ev gwEvent) {
	switch ev.kind {
	case evText:
		t.emitParts([]GeminiPart{{Text: ev.text}})
	case evThinking: // M10-W3：思考 → thought part（Gemini 原生思考形态）
		t.emitParts([]GeminiPart{{Text: ev.text, Thought: true}})
	case evToolStart:
		if ev.name != "" {
			t.toolNames[ev.idx] = ev.name
		}
	case evToolArgs:
		if ev.text != "" {
			t.fnPart(ev.idx, ev.text)
		}
	case evToolDone:
		if !t.toolEmits[ev.idx] { // 名字先到、始终无参数：补空参 part
			t.fnPart(ev.idx, "")
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

// Finish 输出终止事件（finishReason + usageMetadata；无 [DONE]）。
func (t *geminiEncoder) Finish() {
	// 名字先到但流戛然而止的工具：补空参 part（防丢调用）。
	for idx := range t.toolNames {
		if !t.toolEmits[idx] {
			t.fnPart(idx, "")
		}
	}
	final := GeminiResponse{Candidates: []GeminiCandidate{{
		Content: GeminiContent{Role: "model", Parts: nil}, FinishReason: chatFinishToGemini(t.finish),
	}}}
	if t.usage != nil {
		// gemini 出口语义：cachedContentTokenCount 含于 promptTokenCount；
		// thoughtsTokenCount 入 usageMetadata（D4，有则带）。
		final.UsageMeta = &GeminiUsage{
			PromptTokenCount:        t.usage.prompt,
			CandidatesTokenCount:    t.usage.completion,
			CachedContentTokenCount: t.usage.cached,
			ThoughtsTokenCount:      t.usage.reasoning,
		}
	}
	t.out.emit("", final)
}

// ---------- 流式翻译：上游 Gemini SSE → 事件（M5-W1 decoder） ----------

// geminiDecoder 解析上游 Gemini 事件（承接 M4 geminiToChatStream：文本
// 并接、functionCall 的 Index 恒 0（多调用按参数级联——上游形状如此）、
// finishReason/usageMetadata 提取）。工具顺序号沿用 0（与 M4 chat chunk
// tool_calls.index=0 行为一致）。
type geminiDecoder struct {
	hasTool   bool
	sawFinish bool
}

func newGeminiDecoder() *geminiDecoder { return &geminiDecoder{} }

func (d *geminiDecoder) Feed(ev sseEvent) ([]gwEvent, error) {
	if string(ev.Data) == "[DONE]" || len(ev.Data) == 0 {
		return nil, nil
	}
	var gr GeminiResponse
	if err := json.Unmarshal(ev.Data, &gr); err != nil {
		return nil, err
	}
	var out []gwEvent
	for _, cand := range gr.Candidates {
		var texts []string
		first := true
		for _, p := range cand.Content.Parts {
			if p.Text != "" {
				if p.Thought { // 思考文本（M10-W3）→ evThinking
					out = append(out, gwEvent{kind: evThinking, text: p.Text})
					continue
				}
				texts = append(texts, p.Text)
			}
			if p.FunctionCall != nil {
				d.hasTool = true
				args, _ := json.Marshal(p.FunctionCall.Args)
				if first {
					// 首个调用开 idx 0 的工具事件；后续调用仅追加参数（M4 等价）
					out = append(out, gwEvent{
						kind: evToolStart, idx: 0,
						id: "gemini-" + p.FunctionCall.Name, name: p.FunctionCall.Name,
					})
					first = false
				}
				out = append(out, gwEvent{
					kind: evToolArgs, idx: 0, text: orDefault(string(args), "{}"),
				})
			}
		}
		if s := strings.Join(texts, ""); s != "" {
			out = append(out, gwEvent{kind: evText, text: s})
		}
		if cand.FinishReason != "" {
			d.sawFinish = true
			out = append(out, gwEvent{kind: evStop, stop: geminiStopToChat(cand.FinishReason, d.hasTool)})
		}
	}
	if gr.UsageMeta != nil {
		out = append(out, gwEvent{kind: evUsage, usage: gwUsage{
			prompt: gr.UsageMeta.PromptTokenCount, completion: gr.UsageMeta.CandidatesTokenCount,
			cached: gr.UsageMeta.CachedContentTokenCount, reasoning: gr.UsageMeta.ThoughtsTokenCount,
		}})
	}
	return out, nil
}

// Done：Gemini 流以最后一个含 finishReason 的事件收束；未出现过则补
// stop（chat/Messages/Responses 客户端语义的终止兜底）。
func (d *geminiDecoder) Done() []gwEvent {
	if !d.sawFinish {
		return []gwEvent{{kind: evStop, stop: "stop"}}
	}
	return nil
}
