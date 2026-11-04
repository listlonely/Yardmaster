// M10-W2 请求侧多模态矩阵：四种入站图片形态 × 四种上游协议的全组合
// （构造请求，不碰真机）+ D6 留痕断言 + 混排顺序保留。还原 IR v2
// 图片语义（protocol.md「IR v2」节）即失败。
package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

const w2ImgB64 = "iVBORw0KGgo=" // 1x1 PNG 头部截短替身（形状测试不校验像素）

// w2InboundForm 是一种入站图片形态：构造携带一张图片的客户端请求。
type w2InboundForm struct {
	name   string
	client ClientProto
	req    func() any
}

func w2Forms() []w2InboundForm {
	const b64 = w2ImgB64
	const dataURL = "data:image/png;base64," + b64
	const url = "https://example.com/y.png"
	return []w2InboundForm{
		{
			name: "messages-base64", client: ProtoMessages,
			req: func() any {
				return &MessagesRequest{Model: "m", MaxTokens: 100, Messages: []MessagesMessage{{
					Role: "user",
					Content: []any{
						map[string]any{"type": "text", "text": "看图"},
						map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": b64}},
					},
				}}}
			},
		},
		{
			name: "messages-url", client: ProtoMessages,
			req: func() any {
				return &MessagesRequest{Model: "m", MaxTokens: 100, Messages: []MessagesMessage{{
					Role: "user",
					Content: []any{
						map[string]any{"type": "text", "text": "看图"},
						map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}},
					},
				}}}
			},
		},
		{
			name: "chat-dataurl", client: ProtoChat,
			req: func() any {
				return &ChatRequest{Model: "m", Messages: []ChatMessage{{
					Role: "user",
					Content: []any{
						map[string]any{"type": "text", "text": "看图"},
						map[string]any{"type": "image_url", "image_url": dataURL},
					},
				}}}
			},
		},
		{
			name: "chat-object-detail", client: ProtoChat,
			req: func() any {
				return &ChatRequest{Model: "m", Messages: []ChatMessage{{
					Role: "user",
					Content: []any{
						map[string]any{"type": "image_url", "image_url": map[string]any{"url": url, "detail": "high"}},
					},
				}}}
			},
		},
		{
			name: "responses-input-image", client: ProtoResponses,
			req: func() any {
				return &ResponsesRequest{Model: "m", Input: []any{map[string]any{
					"type": "message", "role": "user",
					"content": []any{
						map[string]any{"type": "input_text", "text": "看图"},
						map[string]any{"type": "input_image", "image_url": dataURL, "detail": "low"},
					},
				}}}
			},
		},
		{
			name: "responses-input-image-url", client: ProtoResponses,
			req: func() any {
				return &ResponsesRequest{Model: "m", Input: []any{map[string]any{
					"type": "message", "role": "user",
					"content": []any{map[string]any{"type": "input_image", "image_url": url}},
				}}}
			},
		},
		{
			name: "gemini-inline", client: ProtoGemini,
			req: func() any {
				return &GeminiRequest{Model: "m", Contents: []GeminiContent{{Role: "user", Parts: []GeminiPart{
					{Text: "看图"},
					{InlineData: &GeminiInline{MimeType: "image/png", Data: b64}},
				}}}}
			},
		},
		{
			name: "gemini-filedata", client: ProtoGemini,
			req: func() any {
				return &GeminiRequest{Model: "m", Contents: []GeminiContent{{Role: "user", Parts: []GeminiPart{
					{FileData: &GeminiFile{MimeType: "image/png", FileURI: url}},
				}}}}
			},
		},
	}
}

// TestImageMatrix：每种入站形态 × 每种上游协议，断言图片以该上游的
// 形状到达（Gemini 上游仅内联 base64 可携带——URL 图丢弃+留痕，另测）。
func TestImageMatrix(t *testing.T) {
	const b64 = w2ImgB64
	const dataURL = "data:image/png;base64," + b64
	for _, f := range w2Forms() {
		for _, up := range []ClientProto{ProtoChat, ProtoMessages, ProtoResponses, ProtoGemini} {
			body := translateRequest(f.client, f.req(), up, false)
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("[%s→%s] 上游体非法 JSON: %v", f.name, up, err)
			}
			s := string(body)
			switch up {
			case ProtoChat:
				// chat 上游：分段数组有序保留（string/对象两种 image_url
				// 形态均合法——chat→chat 为合成组合，生产走透传不经此层）
				if !strings.Contains(s, `"type":"image_url"`) {
					t.Errorf("[%s→chat] 缺 image_url 分段: %s", f.name, s)
				}
				if !strings.Contains(s, dataURL) && !strings.Contains(s, "https://example.com/y.png") {
					t.Errorf("[%s→chat] 图片载荷错误: %s", f.name, s)
				}
			case ProtoMessages:
				if !strings.Contains(s, `"type":"image"`) {
					t.Errorf("[%s→messages] 缺 image 块: %s", f.name, s)
				}
				if !strings.Contains(s, `"data":"`+b64+`"`) && !strings.Contains(s, `"url":"https://example.com/y.png"`) {
					t.Errorf("[%s→messages] 图片载荷错误: %s", f.name, s)
				}
			case ProtoResponses:
				if !strings.Contains(s, `"type":"input_image"`) {
					t.Errorf("[%s→responses] 缺 input_image 项: %s", f.name, s)
				}
				if !strings.Contains(s, dataURL) && !strings.Contains(s, `"image_url":"https://example.com/y.png"`) {
					t.Errorf("[%s→responses] 图片载荷错误: %s", f.name, s)
				}
			case ProtoGemini:
				urlForm := f.name == "messages-url" || f.name == "chat-object-detail" ||
					f.name == "responses-input-image-url" || f.name == "gemini-filedata"
				if urlForm {
					// URL 图到 Gemini 上游：丢弃（fileData 需先上传，Magpie 同决策）
					if strings.Contains(s, "inlineData") {
						t.Errorf("[%s→gemini] URL 图不应生成 inlineData: %s", f.name, s)
					}
					continue
				}
				if !strings.Contains(s, `"inlineData":{"mimeType":"image/png","data":"`+b64+`"}`) {
					t.Errorf("[%s→gemini] 缺 inlineData: %s", f.name, s)
				}
			}
		}
	}
}

// TestImageOrderPreserved：text+image+text 混排到 chat/messages 上游，
// 顺序与分块结构保留（不合并、不换位）。
func TestImageOrderPreserved(t *testing.T) {
	const dataURL = "data:image/png;base64," + w2ImgB64
	mr := &MessagesRequest{Model: "m", MaxTokens: 100, Messages: []MessagesMessage{{
		Role: "user",
		Content: []any{
			map[string]any{"type": "text", "text": "前"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": w2ImgB64}},
			map[string]any{"type": "text", "text": "后"},
		},
	}}}
	// → chat：分段数组三段有序
	body := translateRequest(ProtoMessages, mr, ProtoChat, false)
	if !strings.Contains(string(body), `[{"type":"text","text":"前"},{"type":"image_url"`) {
		t.Errorf("chat 上游混排顺序错误: %s", body)
	}
	// → messages：三块有序
	body = translateRequest(ProtoMessages, mr, ProtoMessages, false)
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"messages"`
	}
	_ = out
	if !strings.Contains(string(body), `[{"type":"text","text":"前"},{"type":"image"`) {
		t.Errorf("messages 上游混排顺序错误: %s", body)
	}
}

// TestD6IgnoredNotes：未建模/不可携带块的 fail-closed 留痕（D6）。
func TestD6IgnoredNotes(t *testing.T) {
	// assistant 历史中的 thinking 块 → chat 枢纽：丢弃 + 留痕（D2）
	mr := &MessagesRequest{Model: "m", MaxTokens: 100, Messages: []MessagesMessage{
		{Role: "assistant", Content: []any{
			map[string]any{"type": "thinking", "thinking": "内部推理", "signature": "sig"},
			map[string]any{"type": "text", "text": "答案"},
		}},
		{Role: "user", Content: []any{
			map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "x"}},
		}},
	}}
	cr := messagesToChatRequest(mr)
	joined := strings.Join(cr.Ignored, ",")
	if !strings.Contains(joined, "thinking") || !strings.Contains(joined, "document") {
		t.Errorf("D2/D6 留痕缺失: %v", cr.Ignored)
	}
	if len(cr.Messages) != 1 || cr.Messages[0].Content != "答案" {
		t.Errorf("文本应保留、忽略块不应产生消息: %+v", cr.Messages)
	}

	// URL 图 → gemini 上游：丢弃 + 留痕（fileData 出站不生成）
	cr2 := &ChatRequest{Model: "m", Messages: []ChatMessage{{
		Role: "user",
		Content: []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/y.png"}},
		},
	}}}
	gr := chatToGeminiRequest(cr2)
	if len(gr.Contents) != 0 || len(gr.Ignored) == 0 {
		t.Errorf("URL 图到 gemini 应丢弃并留痕: contents=%v ignored=%v", gr.Contents, gr.Ignored)
	}

	// assistant 图片 → responses 上游：丢弃 + 留痕（output_text 仅文本）
	cr3 := &ChatRequest{Model: "m", Messages: []ChatMessage{{
		Role: "assistant",
		Content: []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/y.png"}},
		},
	}}}
	rr := chatToResponsesRequest(cr3)
	if len(rr.Ignored) == 0 {
		t.Errorf("assistant 图片到 responses 应留痕: %v", rr.Ignored)
	}

	// system 图片：chat 上游 system 仅文本 → 留痕
	mr4 := &MessagesRequest{Model: "m", MaxTokens: 100, System: []any{
		map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/y.png"}},
	}, Messages: []MessagesMessage{{Role: "user", Content: "hi"}}}
	cr4 := messagesToChatRequest(mr4)
	if len(cr4.Ignored) == 0 {
		t.Errorf("system 图片应留痕: %v", cr4.Ignored)
	}
}

// TestTextOnlyUnchanged：纯文本请求经新分块路径与旧 contentText 行为
// 等价（join "\n"、空内容不产生消息、透传路径不经此层）。
func TestTextOnlyUnchanged(t *testing.T) {
	mr := &MessagesRequest{Model: "m", MaxTokens: 100, Messages: []MessagesMessage{
		{Role: "user", Content: "单串"},
		{Role: "user", Content: []any{
			map[string]any{"type": "text", "text": "a"},
			map[string]any{"type": "text", "text": "b"},
		}},
	}}
	cr := messagesToChatRequest(mr)
	if len(cr.Messages) != 2 || cr.Messages[0].Content != "单串" || cr.Messages[1].Content != "a\nb" {
		t.Errorf("纯文本行为漂移: %+v", cr.Messages)
	}
	if len(cr.Ignored) != 0 {
		t.Errorf("纯文本不应有留痕: %v", cr.Ignored)
	}
}
