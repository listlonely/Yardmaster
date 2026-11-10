// IR v2 内容分块（M10-W2）：跨协议图片透传的中间形态与四协议
// 解码/编码。设计定稿见 docs/subsystems/protocol.md「IR v2」节——
// 决策 D3（messages thinking 块仅文本）属响应侧 W3；本文件只做
// 请求侧：text + image 两类，未知块 fail-closed 忽略并返回留痕清单
// （D6：不静默），image 形态归一为 base64(Data+MediaType) 或 URL。
// 校准来源：Magpie chat.go imagePart/chatImage、anthropic.go
// imageBlock、gemini.go gPart、responses.go input_image（v0.1.2890）。
package gateway

import (
	"encoding/json"
	"strings"
)

// IRPart 是一条内容分块的协议无关形态（IR v2 中间形态）。
type IRPart struct {
	Kind      string // "text" | "image"
	Text      string
	MediaType string // image：base64 形态的 MIME（随 Data 成对出现）
	Data      string // image：base64 载荷
	URL       string // image：http(s) URL（无 Data 时）
	Detail    string // image：chat/responses 的 low/high/auto/original（可空）
}

// irImagePart 从 data: URL 解析内联图片（Magpie imagePart 同构）：
// 非 data: 前缀视为普通 URL。
func irImagePart(u string) IRPart {
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		if meta, data, ok := strings.Cut(rest, ","); ok {
			return IRPart{Kind: "image", MediaType: strings.TrimSuffix(meta, ";base64"), Data: data}
		}
	}
	return IRPart{Kind: "image", URL: u}
}

// irDataURL 图片 → chat/responses 的 image_url 值：内联走 data: URL，
// 否则原 URL。
func irDataURL(p IRPart) string {
	if p.Data != "" {
		return "data:" + p.MediaType + ";base64," + p.Data
	}
	return p.URL
}

// irNote 把一条留痕记录追加进请求携带的 Ignored 清单（D6：忽略必留痕）。
func irNote(dst *[]string, note string) {
	*dst = append(*dst, note)
}

// ---------- 入站解码：各协议 → []IRPart ----------

// msgContentParts 把 messages 内容（string 或块数组）解析为 IRPart。
// tool_result 块由调用方先行分离（splitToolResults），此处只处理
// text/image；其余块类型计入 ignored（D6）。
func msgContentParts(c any, ignored *[]string) []IRPart {
	var out []IRPart
	addBlocks := func(blocks []map[string]any) {
		for _, m := range blocks {
			tp, _ := m["type"].(string)
			switch tp {
			case "text":
				if s, _ := m["text"].(string); s != "" {
					out = append(out, IRPart{Kind: "text", Text: s})
				}
			case "tool_result":
				// 结构块：由调用方 splitToolResults 消费，此处静默跳过
			case "image":
				src, _ := m["source"].(map[string]any)
				st, _ := src["type"].(string)
				switch st {
				case "base64":
					mt, _ := src["media_type"].(string)
					d, _ := src["data"].(string)
					if d != "" {
						out = append(out, IRPart{Kind: "image", MediaType: mt, Data: d})
					}
				case "url":
					u, _ := src["url"].(string)
					if u != "" {
						out = append(out, IRPart{Kind: "image", URL: u})
					}
				default:
					irNote(ignored, "image(source=" + orDefault(st, "?") + ")")
				}
			default:
				irNote(ignored, orDefault(tp, "unknown"))
			}
		}
	}
	switch t := c.(type) {
	case string:
		if t != "" {
			out = append(out, IRPart{Kind: "text", Text: t})
		}
	case []any:
		var blocks []map[string]any
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				blocks = append(blocks, m)
			}
		}
		addBlocks(blocks)
	case []Block:
		// hub 内部形态（Blocks 直传）：经 JSON 往返复用 map 解析。
		raw, err := json.Marshal(t)
		if err != nil {
			return out
		}
		var blocks []map[string]any
		if json.Unmarshal(raw, &blocks) == nil {
			addBlocks(blocks)
		}
	}
	return out
}

// noteUnmodeledBlocks 把 assistant 内容中的未建模块计入留痕（D2：thinking
// /redacted_thinking 跨协议丢弃；image/document 等同规）——text/tool_use
// 之外的块类型逐个记名。
func noteUnmodeledBlocks(c any, ignored *[]string) {
	note := func(tp string) {
		if tp != "text" && tp != "tool_use" {
			irNote(ignored, orDefault(tp, "unknown"))
		}
	}
	switch t := c.(type) {
	case []any:
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				tp, _ := m["type"].(string)
				note(tp)
			}
		}
	case []Block:
		for _, blk := range t {
			note(blk.Type)
		}
	}
}

// chatContentParts 把 chat 内容（string 或分段数组）解析为 IRPart。
// image_url 接受字符串或 {url, detail} 对象（Magpie chatParts 同构）。
func chatContentParts(c any, ignored *[]string) []IRPart {
	var out []IRPart
	switch t := c.(type) {
	case string:
		if t != "" {
			out = append(out, IRPart{Kind: "text", Text: t})
		}
	case []any:
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			tp, _ := m["type"].(string)
			switch tp {
			case "text":
				if s, _ := m["text"].(string); s != "" {
					out = append(out, IRPart{Kind: "text", Text: s})
				}
			case "image_url":
				switch v := m["image_url"].(type) {
				case string:
					if v != "" {
						out = append(out, irImagePart(v))
					}
				case map[string]any:
					u, _ := v["url"].(string)
					d, _ := v["detail"].(string)
					if u != "" {
						p := irImagePart(u)
						p.Detail = d
						out = append(out, p)
					}
				}
			default:
				irNote(ignored, orDefault(tp, "unknown"))
			}
		}
	case []ChatContentPart:
		// hub 内部形态（第一步构建的分段直传）
		for _, p := range t {
			switch p.Type {
			case "text":
				if p.Text != "" {
					out = append(out, IRPart{Kind: "text", Text: p.Text})
				}
			case "image_url":
				if p.ImageURL != nil && p.ImageURL.URL != "" {
					ip := irImagePart(p.ImageURL.URL)
					ip.Detail = p.ImageURL.Detail
					out = append(out, ip)
				}
			default:
				irNote(ignored, orDefault(p.Type, "unknown"))
			}
		}
	}
	return out
}

// respContentParts 把 responses message 项的 content 解析为 IRPart
// （input_text/output_text/text/summary_text → 文本；input_image → 图片）。
func respContentParts(c any, ignored *[]string) []IRPart {
	var out []IRPart
	switch t := c.(type) {
	case string:
		if t != "" {
			out = append(out, IRPart{Kind: "text", Text: t})
		}
	case []any:
		for _, part := range t {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			tp, _ := pm["type"].(string)
			switch tp {
			case "input_text", "output_text", "text", "summary_text":
				if s, _ := pm["text"].(string); s != "" {
					out = append(out, IRPart{Kind: "text", Text: s})
				}
			case "input_image":
				u, _ := pm["image_url"].(string)
				if u == "" {
					if inner, ok := pm["image_url"].(map[string]any); ok {
						u, _ = inner["url"].(string)
					}
				}
				if u != "" {
					p := irImagePart(u)
					if d, _ := pm["detail"].(string); d != "" {
						p.Detail = d
					}
					out = append(out, p)
				}
			default:
				irNote(ignored, orDefault(tp, "unknown"))
			}
		}
	}
	return out
}

// geminiPartsOf 把 Gemini parts 解析为 IRPart（inlineData=内联 base64、
// fileData=文件 URI；thought/functionCall/FunctionResponse 由调用方分支
// 处理）。四种形态全部建模，无忽略项。
func geminiPartsOf(parts []GeminiPart) []IRPart {
	var out []IRPart
	for _, p := range parts {
		switch {
		case p.Text != "":
			out = append(out, IRPart{Kind: "text", Text: p.Text})
		case p.InlineData != nil && p.InlineData.Data != "":
			out = append(out, IRPart{Kind: "image", MediaType: p.InlineData.MimeType, Data: p.InlineData.Data})
		case p.FileData != nil && p.FileData.FileURI != "":
			out = append(out, IRPart{Kind: "image", MediaType: p.FileData.MimeType, URL: p.FileData.FileURI})
		}
	}
	return out
}

// ---------- 出站编码：[]IRPart → 各协议形态 ----------

// irToChatContent 把 IRPart 列表编为 chat Content：纯文本合并为字符串
// （与 contentText 的 join 语义逐字节一致，保既有行为），含图片时输出
// 分段数组（text/image_url；data: URL 形态，detail 按原值带上）。
func irToChatContent(ps []IRPart) any {
	if len(ps) == 0 {
		return nil
	}
	allText := true
	for _, p := range ps {
		if p.Kind != "text" {
			allText = false
			break
		}
	}
	if allText {
		var texts []string
		for _, p := range ps {
			texts = append(texts, p.Text)
		}
		return strings.Join(texts, "\n")
	}
	var parts []ChatContentPart
	for _, p := range ps {
		if p.Kind == "text" {
			parts = append(parts, ChatContentPart{Type: "text", Text: p.Text})
			continue
		}
		iu := &ChatImgURL{URL: irDataURL(p)}
		if p.Detail != "" {
			iu.Detail = p.Detail
		}
		parts = append(parts, ChatContentPart{Type: "image_url", ImageURL: iu})
	}
	return parts
}

// irToMsgContent 把 IRPart 列表编为 messages Content：纯文本合并为
// 字符串（同上保形），含图片时输出块数组（image 块 base64/url 两形态，
// Magpie imageBlock 同构）。
func irToMsgContent(ps []IRPart) any {
	if len(ps) == 0 {
		return nil
	}
	allText := true
	for _, p := range ps {
		if p.Kind != "text" {
			allText = false
			break
		}
	}
	if allText {
		var texts []string
		for _, p := range ps {
			texts = append(texts, p.Text)
		}
		return strings.Join(texts, "\n")
	}
	return irToMsgBlocks(ps)
}

func irToMsgBlocks(ps []IRPart) []Block {
	var blocks []Block
	for _, p := range ps {
		if p.Kind == "text" {
			blocks = append(blocks, Block{Type: "text", Text: p.Text})
			continue
		}
		blk := Block{Type: "image"}
		if p.Data != "" {
			blk.Source = &BlockSource{Type: "base64", MediaType: p.MediaType, Data: p.Data}
		} else {
			blk.Source = &BlockSource{Type: "url", URL: p.URL}
		}
		blocks = append(blocks, blk)
	}
	return blocks
}

// irToGeminiParts 把 IRPart 列表编为 Gemini parts：图片仅内联
// inlineData（Magpie 同决策：URL 图丢弃——fileData 需先上传文件），
// URL 图由调用方计入 ignored 留痕。
func irToGeminiParts(ps []IRPart, ignored *[]string) []GeminiPart {
	var parts []GeminiPart
	for _, p := range ps {
		if p.Kind == "text" {
			parts = append(parts, GeminiPart{Text: p.Text})
			continue
		}
		if p.Data == "" {
			irNote(ignored, "image(url，Gemini 无法携带)")
			continue
		}
		parts = append(parts, GeminiPart{InlineData: &GeminiInline{MimeType: p.MediaType, Data: p.Data}})
	}
	return parts
}

// irToRespContent 把 IRPart 列表编为 responses message 项 content：
// role=user 用 input_text/input_image，assistant 用 output_text；纯文本
// 合并为单项（与 respContentText 的 join 语义一致）。
func irToRespContent(ps []IRPart, assistant bool) []any {
	if len(ps) == 0 {
		return nil
	}
	textType := "input_text"
	if assistant {
		textType = "output_text"
	}
	allText := true
	for _, p := range ps {
		if p.Kind != "text" {
			allText = false
			break
		}
	}
	if allText {
		var texts []string
		for _, p := range ps {
			texts = append(texts, p.Text)
		}
		return []any{map[string]any{"type": textType, "text": strings.Join(texts, "\n")}}
	}
	var out []any
	for _, p := range ps {
		if p.Kind == "text" {
			out = append(out, map[string]any{"type": textType, "text": p.Text})
			continue
		}
		im := map[string]any{"type": "input_image", "image_url": irDataURL(p)}
		if p.Detail != "" {
			im["detail"] = p.Detail
		}
		out = append(out, im)
	}
	return out
}
