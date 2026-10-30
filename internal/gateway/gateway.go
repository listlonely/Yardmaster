// Package gateway 实现本地 HTTP 网关：双协议（OpenAI Chat / Anthropic
// Messages）解析与互转、候选计划与故障转移（PRD F1、F6）。
//
// 安全基线（PRD F10，不可协商）：默认仅监听回环地址；响应携带 X-Gw-*
// 头标明实际服务方；上游密钥只经 netproxy，日志脱敏。
//
// W2 边界：网关 key 校验待接入 settings（TODO(P1)）；trace 落盘在 W6；
// 翻译覆盖文本+工具，thinking/多模态块 TODO(P1)。
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"yardmaster/internal/credential"
	"yardmaster/internal/netproxy"
	"yardmaster/internal/provider"
)

// webFS 是控制台静态资源（M2-W1 框架页：总览/Provider/路由/观测），
// embed 进单二进制；/{$} 出入口，/web/ 前缀挂资源。
//
//go:embed web
var webFS embed.FS

// DefaultAddr 是网关默认监听地址（PRD 原型：仅回环，F10）。
const DefaultAddr = "127.0.0.1:8317"

// 上游路径拼接规则：base_url 为完整前缀（如 https://api.deepseek.com）。
const (
	chatUpstreamPath     = "/chat/completions"
	messagesUpstreamPath = "/v1/messages"
)

// restDurations 对应规则页失败分类表：无上游重置时间时的默认休眠。
const (
	restQuota   = 5 * time.Minute // 429/额度：上游未给出重置时间时的默认
	restNetwork = 5 * time.Minute // 连接失败/5xx 短休眠
	restCred    = 30 * time.Second
)

// Server 是网关 HTTP 服务。
type Server struct {
	Addr          string
	Planner       *Planner
	Version       string
	GlobalProxy   string       // 全局代理（F3 继承链末端）；空=系统代理
	Rules         *RuleEngine  // 任务规则（F7）；nil=不启用
	Keys          *KeyStore    // 调用方默认模型绑定；nil=不启用
	Intents       *IntentChain // 意图判定链（F8）；nil=不分类
	Traces        *TraceLog    // 决策追溯（F9）；nil=不记录
	AppDir        string       // 数据目录（管理 API 备份/写配置用）；空=禁用写接口
	ProvidersPath string       // providers.json 路径（/api/reload 热重载用）
	Token         string       // 访问令牌（M2-W5）：非空时 /v1 与 /api 要求匹配；空=本机信任态
	Creds         *credential.Store // OAuth 凭证库（M3-W3）；nil=无 OAuth 能力
	oauth         oauthRuntime      // OAuth 运行态（token 缓存/single-flight）
	started       time.Time
}

// Handler 返回网关路由表。
func (s *Server) Handler() http.Handler {
	s.started = time.Now()
	return s.authMiddleware(s.routes())
}

// routes 是裸路由表（authMiddleware 的内层）。
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"name": "yardmaster", "status": "ok"})
	})
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleInference(ProtoChat))
	mux.HandleFunc("POST /v1/messages", s.handleInference(ProtoMessages))
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/trace", s.handleTrace)
	mux.HandleFunc("GET /api/config/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.handleConfigGet(w, r.PathValue("name"))
	})
	mux.HandleFunc("PUT /api/config/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.handleConfigPut(w, r, r.PathValue("name"))
	})
	mux.HandleFunc("POST /api/reload", s.handleReload)
	mux.HandleFunc("GET /api/cost", s.handleCost)
	mux.Handle("GET /web/", http.StripPrefix("/web/", http.FileServerFS(webSub())))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		index, err := fs.ReadFile(webFS, "web/index.html")
		if err != nil {
			http.Error(w, "控制台资源缺失", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
	return mux
}

// webSub 返回 web/ 子文件系统（资源路径去掉目录前缀）。
func webSub() fs.FS {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("gateway: embed web 子目录缺失: " + err.Error())
	}
	return sub
}

// Serve 阻塞运行网关，ctx 取消时优雅关闭（暂停网关语义见 PRD F1）。
func (s *Server) Serve(ctx context.Context) error {
	addr := s.Addr
	if addr == "" {
		addr = DefaultAddr
	}
	ln, err := newLoopbackListener(addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// newLoopbackListener 监听 addr（F10：默认回环；局域网共享为 P1 开关）。
func newLoopbackListener(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

// ---------- 端点 ----------

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	models := []map[string]any{}
	if s.Planner != nil {
		for _, id := range s.Planner.AllModels() {
			models = append(models, map[string]any{"id": id, "object": "model"})
		}
	}
	writeJSON(w, map[string]any{"object": "list", "data": models})
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	state := map[string]any{
		"version": s.Version,
		"uptime":  time.Since(s.started).Round(time.Second).String(),
	}
	if s.Planner != nil {
		// 全部快照一次性锁内复制（StateSnapshot），消除对 Planner 内部
		// 切片与账号字段的无锁遍历（CI -race 实测数据竞争）。
		provs, accs, groups := s.Planner.StateSnapshot()
		if provs != nil {
			state["providers"] = provs
		}
		if accs != nil {
			state["accounts"] = accs
		}
		if groups != nil {
			state["groups"] = groups
		}
		resting := map[string]string{}
		for k, until := range s.Planner.Resting() {
			resting[k] = until.Format(time.RFC3339)
		}
		state["resting"] = resting
		// 额度耗尽账号（M2-W4）：读侧计算，与休眠表独立展示。
		if ex := s.Planner.Exhausted(); len(ex) > 0 {
			state["exhausted"] = ex
		}
	}
	if s.Rules != nil {
		state["rules"] = s.Rules.Snapshot()
	}
	writeJSON(w, state)
}

// handleTrace 返回最近的决策记录（F9 观测数据源，管理页后续接入）。
// 查询参数：limit（默认 50）、agent（caller 前缀）、result（ok/fail/switch）。
func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	if s.Traces == nil {
		writeJSON(w, map[string]any{"traces": []any{}})
		return
	}
	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= traceMemCap {
		limit = n
	}
	traces := s.Traces.Recent(limit, r.URL.Query().Get("agent"), r.URL.Query().Get("result"))
	if traces == nil {
		traces = []Trace{}
	}
	writeJSON(w, map[string]any{"traces": traces})
}

// handleInference 返回一个协议端点处理器：解析 → 路由判定（规则→
// 请求模型→key 绑定）→ 候选计划 → 候选循环。
//
// 模型判定顺序（W5 设计决策）：任务规则命中 → 规则 target 覆盖；
// 未命中用客户端请求的 model；model 为空或 "default" → keys.json
// 项目绑定默认模型；仍无目标 → 404。
func (s *Server) handleInference(client ClientProto) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			writeClientErr(client, w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
			return
		}
		var model string
		var stream bool
		var clientReq any
		switch client {
		case ProtoMessages:
			var mr MessagesRequest
			if err := json.Unmarshal(body, &mr); err != nil {
				writeClientErr(client, w, http.StatusBadRequest, "解析 Messages 请求失败: "+err.Error())
				return
			}
			model, stream, clientReq = mr.Model, mr.Stream, &mr
		default:
			var cr ChatRequest
			if err := json.Unmarshal(body, &cr); err != nil {
				writeClientErr(client, w, http.StatusBadRequest, "解析 Chat 请求失败: "+err.Error())
				return
			}
			model, stream, clientReq = cr.Model, cr.Stream, &cr
		}
		if s.Planner == nil {
			writeClientErr(client, w, http.StatusServiceUnavailable, "网关未配置任何 provider")
			return
		}

		caller := callerKeyOf(r)
		sticky := r.Header.Get("X-Gw-Session")
		if sticky == "" {
			sticky = caller + "|" + model // 缺省会话键：调用方 × 请求模型
		}
		// 决策追溯（F9）：从路由判定起记录，解析失败/全败也留痕。
		tr := Trace{ID: traceID(start), Time: start.Format(time.RFC3339), Caller: caller, RequestModel: model}
		recordFail := func(status int) {
			tr.Outcome, tr.HTTPStatus, tr.TotalMS = "fail", status, time.Since(start).Milliseconds()
			if s.Traces != nil {
				s.Traces.Record(tr)
			}
		}

		// 路由判定。内部 L1 分类调用（带标记头）跳过分类与规则，
		// 防止「分类调用再次触发分类」的递归。
		target := model
		if r.Header.Get(L1SelfHeader) != L1SelfValue {
			lastMsg, estTokens := inputStats(clientReq)
			intent := s.Intents.Classify(lastMsg)
			tr.Intent = intent
			if s.Rules != nil {
				if hit, ok := s.Rules.Match(caller, intent, lastMsg, estTokens); ok {
					slog.Info("任务规则命中", "rule", hit.ID, "target", hit.Target,
						"intent", intent, "caller", caller)
					target = hit.Target
					tr.Rule, tr.RoutePath, tr.Target = hit.ID, "rule", hit.Target
					w.Header().Set("X-Gw-Rule", hit.ID)
				}
			}
		}
		if target == "" || target == "default" {
			if bound := s.Keys.DefaultModel(caller); bound != "" {
				target = bound
				tr.RoutePath = "key-default"
			}
		}
		if tr.RoutePath == "" {
			tr.RoutePath = "request"
		}
		if target == "" {
			tr.RoutePath = "none"
			st, b := writeErr(client, http.StatusNotFound,
				"未指定模型且调用方无默认模型绑定（用 `yardmaster keys bind` 配置）")
			writeRaw(w, st, b)
			recordFail(st)
			return
		}
		if tr.Target == "" {
			tr.Target = target
		}

		cands, perr := s.Planner.Plan(target, sticky)
		if perr != nil {
			st, b := writeErr(client, http.StatusNotFound, perr.Error())
			writeRaw(w, st, b)
			recordFail(st)
			return
		}
		if len(cands) == 0 {
			writeClientErr(client, w, http.StatusServiceUnavailable,
				"目标 "+target+" 没有可用候选（停用、凭证失败或组内成员全部不可用）")
			recordFail(http.StatusServiceUnavailable)
			return
		}
		s.serveWithFailover(w, r, client, clientReq, body, stream, cands, sticky, &tr, start)
	}
}

// rewriteModel 把请求体 JSON 的 model 字段改写为上游真实模型名。
// UseNumber 解码避免大整数经 float64 失真；其余字段原样保留。
func rewriteModel(body []byte, upstreamModel string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("请求体不是 JSON 对象")
	}
	m["model"] = upstreamModel
	return json.Marshal(m)
}

// ---------- 候选循环与故障转移 ----------

// attemptResult 是单次尝试的结果。
type attemptResult struct {
	resp        *http.Response // 2xx 时非 nil
	status      int
	body        []byte // 失败时已排干的响应体（全败时原样回传最后一次）
	err         error  // 网络层错误
	clientFault bool   // 请求体本地处理失败（如模型名改写）：不再换候选
}

func (s *Server) serveWithFailover(w http.ResponseWriter, r *http.Request,
	client ClientProto, clientReq any, rawBody []byte, stream bool,
	cands []Candidate, stickyKey string, tr *Trace, start time.Time,
) {
	var last attemptResult
	for _, cand := range cands {
		t0 := time.Now()
		res := s.attempt(r.Context(), client, clientReq, rawBody, stream, cand)
		dur := time.Since(t0).Milliseconds()
		// 候选尝试链留痕（F9）：结果、状态码、失败原因、耗时、出口
		if tr != nil {
			tc := TraceCandidate{
				Provider: cand.Provider.ID, Account: cand.Account.Name,
				Model: cand.UpstreamModel, DurMS: dur,
				Result: classifyAttempt(res), Status: res.status,
			}
			if _, proxyRef, perr := s.resolveProxyRef(cand); perr == nil {
				tc.Proxy = redactProxy(proxyRef)
			}
			if res.err != nil {
				tc.Err = res.err.Error()
			} else if res.status >= 400 {
				tc.Err = netproxy.Snippet(res.body)
			}
			tr.Candidates = append(tr.Candidates, tc)
		}
		if res.clientFault {
			// 请求体本身处理失败：换候选结果相同，原样返回
			s.finishTrace(tr, "fail", res.status, start, nil, false)
			s.relayRaw(w, cand, res)
			return
		}
		if res.err != nil {
			slog.Warn("上游网络失败，切下一候选", "provider", cand.Provider.ID, "account", cand.Account.Name, "err", res.err)
			s.Planner.Rest(cand, restNetwork)
			last = res
			continue
		}
		switch {
		case res.status == http.StatusTooManyRequests:
			s.Planner.Rest(cand, restQuota)
			last = res
			continue
		case res.status >= 500:
			s.Planner.Rest(cand, restNetwork)
			last = res
			continue
		case res.status == http.StatusUnauthorized || res.status == http.StatusForbidden:
			// 凭证失败（W3 分流）：api_key 账号标记 bad-credential（规则页：
			// 切走、需人工处理、不自动重试）；OAuth 账号触发被动刷新
			// （single-flight，异步），短休眠后刷新成功即自动回归。
			if cand.Account.Auth == provider.AuthOAuth {
				s.triggerRefresh(cand)
			} else {
				s.Planner.MarkBadCredByKey(cand.Key())
			}
			s.Planner.Rest(cand, restCred)
			last = res
			continue
		case res.status >= 400:
			// 请求本身的问题：换账号结果相同，原样回给调用方（规则页）
			s.finishTrace(tr, "fail", res.status, start, nil, false)
			s.relayRaw(w, cand, res)
			return
		}
		// 2xx：成功，开始中继；此后不再切换（响应已开始，规则页）。
		// 记录会话亲和：同一会话下次请求优先本候选（醒着才生效）。
		s.Planner.Clear(cand)
		s.Planner.NoteAffinity(stickyKey, cand.Key())
		if tr != nil {
			tr.Served, tr.ServedModel = cand.Key(), cand.UpstreamModel
			tr.TTFBMS = time.Since(start).Milliseconds() // 首字节=上游 2xx 响应头到达
		}
		usage := s.relay(w, client, clientReq, cand, res, stream)
		switched := tr != nil && len(tr.Candidates) > 1
		s.finishTrace(tr, "ok", res.status, start, usage, switched)
		// 当日用量累计（W6）：smart 策略与额度观测的数据源。
		if usage != nil {
			s.Planner.NoteUsage(cand, usage.Prompt+usage.Completion)
		}
		return
	}
	// 全部候选失败：最后一次的原始错误回给 Agent（不伪装 200，规则页）
	status := http.StatusBadGateway
	if last.status >= 400 {
		status = last.status
	}
	s.finishTrace(tr, "fail", status, start, nil, false)
	s.relayLastError(w, client, last)
}

// classifyAttempt 把尝试结果映射为 trace 候选结果分类。
func classifyAttempt(res attemptResult) string {
	switch {
	case res.clientFault:
		return CandClient
	case res.err != nil:
		return CandNet
	case res.status == http.StatusTooManyRequests:
		return CandQuota
	case res.status >= 500:
		return CandServer
	case res.status == http.StatusUnauthorized || res.status == http.StatusForbidden:
		return CandCred
	case res.status >= 400:
		return CandClient
	default:
		return CandOK
	}
}

// finishTrace 补全 trace 的终态并落盘（Traces 未装配时为空操作）。
func (s *Server) finishTrace(tr *Trace, outcome string, status int, start time.Time,
	usage *TraceUsage, switched bool) {
	if tr == nil {
		return
	}
	tr.Outcome, tr.HTTPStatus = outcome, status
	tr.TotalMS = time.Since(start).Milliseconds()
	tr.Usage, tr.Switched = usage, switched
	if s.Traces != nil {
		s.Traces.Record(*tr)
	}
}

// attempt 向单个候选发起上游请求；只负责把响应带回来（含失败分类所需
// 的 status/body），不做任何客户端写入。
//
// 模型名改写按候选进行（W5：组候选可跨 provider、各候选上游模型名
// 不同）：同协议透传路径改写原始字节；翻译路径改写请求结构体。
// W3 真实冒烟发现：透传不改写会让上游收到带 provider 前缀的名字。
func (s *Server) attempt(ctx context.Context, client ClientProto, clientReq any,
	rawBody []byte, stream bool, cand Candidate) attemptResult {
	upstreamProto := clientProtoOf(cand.Provider.Protocol)
	var upBody []byte
	if upstreamProto == client {
		b, merr := rewriteModel(rawBody, cand.UpstreamModel)
		if merr != nil {
			st, body := writeErr(client, http.StatusBadRequest, "改写模型名失败: "+merr.Error())
			return attemptResult{clientFault: true, status: st, body: body}
		}
		upBody = b
	} else {
		switch cq := clientReq.(type) {
		case *ChatRequest:
			cq.Model = cand.UpstreamModel
		case *MessagesRequest:
			cq.Model = cand.UpstreamModel
		}
		upBody = translateRequest(client, clientReq, upstreamProto, stream)
	}
	var path string
	if upstreamProto == ProtoMessages {
		path = messagesUpstreamPath
	} else {
		path = chatUpstreamPath
	}
	url := strings.TrimRight(cand.Provider.BaseURL, "/") + path

	trIf, _, terr := s.resolveProxyRef(cand)
	if terr != nil {
		return attemptResult{err: terr}
	}
	if trIf == nil {
		trIf = http.DefaultTransport
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(upBody))
	if err != nil {
		return attemptResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	// 凭证注入（M3-W3）：oauth 账号经凭证库/缓存取 access token；
	// api_key 账号沿用配置值。读不懂=未知：凭证取不到按失败休眠切换。
	tok, terr := s.upstreamToken(cand)
	if terr != nil {
		return attemptResult{err: terr}
	}
	if upstreamProto == ProtoMessages {
		if cand.Account.Auth == provider.AuthOAuth {
			// Claude OAuth 订阅走 Bearer（非 x-api-key），带 oauth beta 头
			req.Header.Set("Authorization", "Bearer "+tok)
			req.Header.Set("anthropic-beta", "oauth-2025-04-20")
		} else {
			req.Header.Set("x-api-key", tok)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := (&http.Client{Transport: trIf}).Do(req)
	if err != nil {
		return attemptResult{err: err}
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return attemptResult{status: resp.StatusCode, body: b}
	}
	return attemptResult{resp: resp, status: resp.StatusCode}
}

// resolveProxyRef 解析账号→Provider→全局继承链（F3）并返回共享
// RoundTripper（同端点复用连接池，HTTP/2 健康检查见 netproxy）与
// 实际代理出口（trace 记录用，调用方负责脱敏）。
func (s *Server) resolveProxyRef(cand Candidate) (http.RoundTripper, string, error) {
	ref, _ := ResolveProxyRef(string(cand.Account.Proxy), string(cand.Provider.Proxy), s.GlobalProxy)
	rt, err := netproxy.SharedTransport(netproxy.Ref(ref))
	return rt, ref, err
}

// ---------- 中继 ----------

// relay 把 2xx 响应回给客户端；同协议透传，跨协议翻译。返回从上游
// 响应提取的 token 用量（不可得时 nil；W6 用量累计与 trace 数据源）。
func (s *Server) relay(w http.ResponseWriter,
	client ClientProto, clientReq any, cand Candidate, res attemptResult, stream bool) *TraceUsage {
	defer res.resp.Body.Close()
	upstreamProto := clientProtoOf(cand.Provider.Protocol)
	sc := &usageScanner{} // usage 提取（流式逐块 / 非流式整体）

	h := w.Header()
	h.Set("X-Gw-Provider", cand.Provider.ID)
	h.Set("X-Gw-Account", cand.Account.Name)
	h.Set("X-Gw-Model", cand.UpstreamModel)

	if upstreamProto == client {
		if stream {
			h.Set("Content-Type", "text/event-stream; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			fw := &flushWriter{w: w}
			_, _ = io.Copy(fw, io.TeeReader(res.resp.Body, sc))
			fw.flush()
			return sc.usageOf()
		}
		h.Set("Content-Type", orDefault(res.resp.Header.Get("Content-Type"), "application/json"))
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, io.TeeReader(res.resp.Body, sc))
		return sc.usageOf()
	}

	// 跨协议：翻译后输出
	if stream {
		h.Set("Content-Type", sseContentType(client))
		w.WriteHeader(http.StatusOK)
		fw := &flushWriter{w: w}
		s.translateStream(w, fw, client, upstreamProto, io.TeeReader(res.resp.Body, sc))
		fw.flush()
		return sc.usageOf()
	}
	b, err := io.ReadAll(io.LimitReader(res.resp.Body, 64<<20))
	if err != nil {
		writeClientErr(client, w, http.StatusBadGateway, "读取上游响应失败: "+err.Error())
		return nil
	}
	_, _ = sc.Write(b)
	out, oerr := translateResponse(client, upstreamProto, clientReq, b)
	if oerr != nil {
		writeClientErr(client, w, http.StatusBadGateway, oerr.Error())
		return nil
	}
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	return sc.usageOf()
}

// relayRaw 把 4xx（请求本身的问题）原样回给客户端。
func (s *Server) relayRaw(w http.ResponseWriter, cand Candidate, res attemptResult) {
	w.Header().Set("X-Gw-Provider", cand.Provider.ID)
	w.Header().Set("X-Gw-Account", cand.Account.Name)
	ct := ""
	if res.resp != nil {
		defer res.resp.Body.Close()
		ct = res.resp.Header.Get("Content-Type")
		if res.body == nil {
			b, _ := io.ReadAll(io.LimitReader(res.resp.Body, 1<<20))
			res.body = b
		}
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(res.status)
	_, _ = w.Write(res.body)
}

// relayLastError 全部候选失败：最后一次的原始错误回给 Agent（规则页）。
func (s *Server) relayLastError(w http.ResponseWriter, client ClientProto, last attemptResult) {
	switch {
	case last.err != nil:
		writeClientErr(client, w, http.StatusBadGateway, "全部候选失败（最后一次: "+last.err.Error()+"）")
	case last.status >= 400:
		st := last.status
		if st < 400 {
			st = http.StatusBadGateway
		}
		w.WriteHeader(st)
		_, _ = w.Write(last.body)
	default:
		writeClientErr(client, w, http.StatusBadGateway, "全部候选失败")
	}
}

// translateStream 逐事件把上游 SSE 翻译为客户端协议的 SSE。
func (s *Server) translateStream(w http.ResponseWriter, fw *flushWriter,
	client, upstream ClientProto, body io.Reader) {
	br := bufio.NewReaderSize(body, 64<<10)
	switch {
	case upstream == ProtoChat && client == ProtoMessages:
		t := newChatToMsgsStream()
		for {
			ev, err := scanSSE(br)
			if err == io.EOF {
				break
			}
			if err != nil {
				slog.Warn("上游 SSE 读取失败", "err", err)
				break
			}
			if err := t.Feed(ev.Data); err != nil {
				slog.Warn("流式翻译失败", "err", err)
				break
			}
			if t.out != nil {
				_, _ = w.Write(t.out)
				t.out = nil
				fw.flush()
			}
		}
		_, _ = w.Write(t.Done())
		fw.flush()
	case upstream == ProtoMessages && client == ProtoChat:
		t := newMsgsToChatStream()
		for {
			ev, err := scanSSE(br)
			if err == io.EOF {
				break
			}
			if err != nil {
				slog.Warn("上游 SSE 读取失败", "err", err)
				break
			}
			if err := t.Feed(ev); err != nil {
				slog.Warn("流式翻译失败", "err", err)
				break
			}
			if t.out != nil {
				_, _ = w.Write(t.out)
				t.out = nil
				fw.flush()
			}
		}
		_, _ = w.Write(t.Done())
		fw.flush()
	default:
		// 理论不可达：同协议在 relay 已分流
		_, _ = io.Copy(fw, body)
		fw.flush()
	}
}

// ---------- 小工具 ----------

type flushWriter struct {
	w http.ResponseWriter
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

func (f *flushWriter) flush() {
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
}

// clientProtoOf 把 provider.Protocol 转为网关内部协议标识。
func clientProtoOf(p provider.Protocol) ClientProto {
	if string(p) == string(ProtoMessages) {
		return ProtoMessages
	}
	return ProtoChat
}

func sseContentType(client ClientProto) string {
	return "text/event-stream; charset=utf-8"
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func translateRequest(client ClientProto, clientReq any, upstream ClientProto, stream bool) []byte {
	switch {
	case client == ProtoMessages && upstream == ProtoChat:
		mr := clientReq.(*MessagesRequest)
		mr.Stream = stream
		b, _ := json.Marshal(messagesToChatRequest(mr))
		return b
	case client == ProtoChat && upstream == ProtoMessages:
		cr := clientReq.(*ChatRequest)
		cr.Stream = stream
		b, _ := json.Marshal(chatToMessagesRequest(cr))
		return b
	default:
		b, _ := json.Marshal(clientReq)
		return b
	}
}

func translateResponse(client, upstream ClientProto, clientReq any, upstreamBody []byte) ([]byte, error) {
	switch {
	case client == ProtoChat && upstream == ProtoMessages:
		var ms MessagesResponse
		if err := json.Unmarshal(upstreamBody, &ms); err != nil {
			return nil, fmt.Errorf("解析上游 messages 响应: %w", err)
		}
		return json.Marshal(messagesResponseToChat(ms))
	case client == ProtoMessages && upstream == ProtoChat:
		var cc ChatChunk
		if err := json.Unmarshal(upstreamBody, &cc); err != nil {
			return nil, fmt.Errorf("解析上游 chat 响应: %w", err)
		}
		mr := clientReq.(*MessagesRequest)
		return json.Marshal(chatResponseToMessages(mr, &cc))
	default:
		return upstreamBody, nil
	}
}

// messagesResponseToChat 把上游 messages 非流响应翻译为客户端 chat 响应。
func messagesResponseToChat(ms MessagesResponse) *ChatChunk {
	var out ChatChunk
	out.ID = ms.ID
	out.Object = "chat.completion"
	out.Model = ms.Model
	out.Usage = &ChatUsage{PromptTokens: ms.Usage.InputTokens, CompletionTokens: ms.Usage.OutputTokens}
	choice := ChatChoice{Index: 0, FinishReason: msgStopToChat(ms.StopReason)}
	msg := &ChatMessage{Role: "assistant"}
	var texts []string
	for _, blk := range ms.Content {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				texts = append(texts, blk.Text)
			}
		case "tool_use":
			args, _ := json.Marshal(blk.Input)
			msg.ToolCalls = append(msg.ToolCalls, ChatToolCall{
				ID:       blk.ID,
				Type:     "function",
				Function: ChatFunction{Name: blk.Name, Arguments: string(args)},
			})
		}
	}
	msg.Content = strings.Join(texts, "")
	choice.Message = msg
	out.Choices = append(out.Choices, choice)
	return &out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeClientErr(client ClientProto, w http.ResponseWriter, status int, msg string) {
	st, body := writeErr(client, status, msg)
	writeRaw(w, st, body)
}
