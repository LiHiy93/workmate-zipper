package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/LiHiy93/workmate-zipper/internal/task"
)

type Server struct {
	mux     *http.ServeMux
	manager *task.Manager
}

func NewServer(m *task.Manager) http.Handler {
	s := &Server{mux: http.NewServeMux(), manager: m}
	s.routes()
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /tasks", s.handleCreateTask)
	s.mux.HandleFunc("POST /tasks/{id}/items", s.handleAddItem)
	s.mux.HandleFunc("POST /tasks/{id}/run", s.handleRun)
	s.mux.HandleFunc("GET /tasks/{id}/status", s.handleStatus)
	s.mux.HandleFunc("GET /tasks/{id}/result", s.handleResult)
	files := http.StripPrefix("/files/", http.FileServer(http.Dir("results")))
	s.mux.Handle("/files/", files)
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	t := s.manager.Create()
	writeJSON(w, http.StatusCreated, map[string]string{"id": t.ID})
}

type addItemReq struct {
	URL string `json:"url"`
}

func (s *Server) handleAddItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in addItemReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid json")
		return
	}
	in.URL = strings.TrimSpace(in.URL)
	if in.URL == "" {
		httpError(w, http.StatusBadRequest, "empty url")
		return
	}
	added, limit, err := s.manager.AddItem(id, in.URL)
	if err != nil {
		switch {
		case errors.Is(err, task.ErrNotFound):
			httpError(w, http.StatusNotFound, "task not found")
		case errors.Is(err, task.ErrTooManyItems):
			httpError(w, http.StatusBadRequest, "items limit reached")
		case errors.Is(err, task.ErrAlreadyStarted):
			httpError(w, http.StatusConflict, "task already started")
		case errors.Is(err, task.ErrUnsupportedType):
			httpError(w, http.StatusBadRequest, "only .pdf and .jpeg are allowed")
		default:
			httpError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "limit": limit})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.manager.Run(id, r.Context())
	if err != nil {
		switch {
		case errors.Is(err, task.ErrNotFound):
			httpError(w, http.StatusNotFound, "task not found")
		case errors.Is(err, task.ErrBusy):
			httpError(w, http.StatusConflict, "server is busy, try later")
		case errors.Is(err, task.ErrNoItems):
			httpError(w, http.StatusBadRequest, "no valid items to process")
		case errors.Is(err, task.ErrAlreadyStarted):
			httpError(w, http.StatusConflict, "task already started")
		default:
			httpError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st := s.manager.Status(id)
	if st == nil {
		httpError(w, http.StatusNotFound, "task not found")
		return
	}
	resp := map[string]any{"status": st.Status, "added": st.Added, "done": st.Done, "error": st.Error, "result_url": ""}
	if st.ResultPath != "" {
		filename := filepath.Base(st.ResultPath)
		resp["result_url"] = "/files/" + filename
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st := s.manager.Status(id)
	if st == nil || st.ResultPath == "" {
		httpError(w, http.StatusNotFound, "result not ready")
		return
	}
	http.ServeFile(w, r, st.ResultPath)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
