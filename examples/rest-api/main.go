// Package main 演示一个纯标准库的 REST API 服务（Go 1.22+ 新路由语法）。
//
// 涵盖知识点：
//   - net/http 服务器与路由（Go 1.22 增强版 ServeMux：方法 + 路径参数）
//   - JSON 请求/响应编解码
//   - RESTful CRUD（内存存储，带互斥锁线程安全）
//   - 中间件模式（日志、恢复 panic）
//   - 优雅关闭（graceful shutdown + signal）
//
// 运行：go run rest_api.go
// 测试：
//
//	curl -s localhost:8080/api/notes
//	curl -s -X POST localhost:8080/api/notes -d '{"title":"hello","body":"world"}'
//	curl -s localhost:8080/api/notes/1
//	curl -s -X PUT localhost:8080/api/notes/1 -d '{"title":"updated"}'
//	curl -s -X DELETE localhost:8080/api/notes/1
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// ============================================================
// 1. 数据模型
// ============================================================

// Note 是一条笔记资源。
type Note struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// ============================================================
// 2. 线程安全的内存存储（Repository 模式）
// ============================================================

type Store struct {
	mu    sync.RWMutex
	notes map[int64]*Note
	next  int64
}

func NewStore() *Store {
	return &Store{notes: make(map[int64]*Note), next: 1}
}

func (s *Store) List() []*Note {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Note, 0, len(s.notes))
	for _, n := range s.notes {
		out = append(out, n)
	}
	return out
}

func (s *Store) Get(id int64) (*Note, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.notes[id]
	return n, ok
}

func (s *Store) Create(title, body string) *Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := &Note{ID: s.next, Title: title, Body: body, CreatedAt: time.Now()}
	s.notes[n.ID] = n
	s.next++
	return n
}

func (s *Store) Update(id int64, title, body string) (*Note, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.notes[id]
	if !ok {
		return nil, false
	}
	if title != "" {
		n.Title = title
	}
	if body != "" {
		n.Body = body
	}
	return n, true
}

func (s *Store) Delete(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.notes[id]; !ok {
		return false
	}
	delete(s.notes, id)
	return true
}

// ============================================================
// 3. HTTP 处理器
// ============================================================

type Server struct {
	store *Store
}

// writeJSON 统一输出 JSON。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 统一输出错误。
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// handleList  GET /api/notes
func (srv *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, srv.store.List())
}

// handleGet   GET /api/notes/{id}
func (srv *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	n, ok := srv.store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "note not found")
		return
	}
	writeJSON(w, http.StatusOK, n)
}

// handleCreate POST /api/notes
func (srv *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if in.Title == "" {
		writeError(w, http.StatusUnprocessableEntity, "title is required")
		return
	}
	n := srv.store.Create(in.Title, in.Body)
	writeJSON(w, http.StatusCreated, n)
}

// handleUpdate PUT /api/notes/{id}
func (srv *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	n, ok := srv.store.Update(id, in.Title, in.Body)
	if !ok {
		writeError(w, http.StatusNotFound, "note not found")
		return
	}
	writeJSON(w, http.StatusOK, n)
}

// handleDelete DELETE /api/notes/{id}
func (srv *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if !srv.store.Delete(id) {
		writeError(w, http.StatusNotFound, "note not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pathID 从路径中提取 {id}。
func pathID(r *http.Request) (int64, error) {
	raw := r.PathValue("id") // Go 1.22+ ServeMux 路径参数
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

// ============================================================
// 4. 中间件（日志 + panic 恢复）
// ============================================================

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Microsecond))
	})
}

func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC: %v", rec)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ============================================================
// 5. main：路由注册 + 优雅关闭
// ============================================================

func main() {
	srv := &Server{store: NewStore()}

	mux := http.NewServeMux()
	// Go 1.22+ 支持在模式里直接写方法与路径参数
	mux.HandleFunc("GET /api/notes", srv.handleList)
	mux.HandleFunc("GET /api/notes/{id}", srv.handleGet)
	mux.HandleFunc("POST /api/notes", srv.handleCreate)
	mux.HandleFunc("PUT /api/notes/{id}", srv.handleUpdate)
	mux.HandleFunc("DELETE /api/notes/{id}", srv.handleDelete)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	handler := withRecovery(withLogging(mux))

	httpSrv := &http.Server{
		Addr:              ":8080",
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// 启动服务（非阻塞）
	go func() {
		log.Println("server listening on :8080")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	// 等待中断信号，优雅关闭（最多等 10s 让在途请求完成）
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
	log.Println("bye")
}
