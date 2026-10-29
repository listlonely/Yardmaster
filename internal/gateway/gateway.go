// Package gateway 实现本地 HTTP 网关：多协议解析与互转、候选计划与
// 故障转移（PRD F1、F6）。M1 协议矩阵只做 OpenAI Chat 与 Anthropic
// Messages（设计文档 §6.2）。
//
// 安全基线（PRD F10，不可协商）：默认仅监听回环地址；每个接入 Agent
// 使用独立网关 key（gw-<agent>）识别调用方；响应携带 X-Gw-* 头标明
// 实际服务方。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// DefaultAddr 是网关默认监听地址（PRD 原型：仅回环，F10）。
const DefaultAddr = "127.0.0.1:8317"

// Server 是网关 HTTP 服务。
type Server struct {
	Addr string
	srv  *http.Server
}

// Handler 返回网关路由表。骨架版：健康检查 + 协议端点占位。
// W2 起逐个替换为真实协议处理与候选计划（F6）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"name": "agenthub", "status": "ok"})
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		// TODO(W2): 由 provider catalog 生成；骨架返回空列表。
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
	})
	mux.HandleFunc("POST /v1/chat/completions", notImplemented)
	mux.HandleFunc("POST /v1/messages", notImplemented)
	return mux
}

// Serve 阻塞运行网关，ctx 取消时优雅关闭（暂停网关语义见 PRD F1）。
func (s *Server) Serve(ctx context.Context) error {
	addr := s.Addr
	if addr == "" {
		addr = DefaultAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.srv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.srv.Shutdown(shutdownCtx)
	}
}

func notImplemented(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{
		"error": "protocol translation lands in M1-W2",
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
