package task

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound        = errors.New("task not found")
	ErrTooManyItems    = errors.New("too many items")
	ErrAlreadyStarted  = errors.New("task already started")
	ErrBusy            = errors.New("too many tasks running")
	ErrNoItems         = errors.New("no items")
	ErrUnsupportedType = errors.New("unsupported type")
)

type Status string

const (
	StatusNew     Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusError   Status = "error"
)

type Task struct {
	ID         string
	Items      []string
	Started    bool
	Status     Status
	Error      string
	ResultPath string
	Added      int
	Done       int
	mu         sync.Mutex
}

type Manager struct {
	mu      sync.RWMutex
	tasks   map[string]*Task
	sem     chan struct{}
	client  *http.Client
	tmpDir  string
	outDir  string
	limitMB int64
}

func NewManager(parallel int) *Manager {
	_ = os.MkdirAll("tmp", 0o755)
	_ = os.MkdirAll("results", 0o755)
	return &Manager{
		tasks:   make(map[string]*Task),
		sem:     make(chan struct{}, parallel),
		tmpDir:  "tmp",
		outDir:  "results",
		client:  &http.Client{Timeout: 15 * time.Second},
		limitMB: 25,
	}
}

func (m *Manager) Create() *Task {
	id := newID()
	t := &Task{ID: id, Status: StatusNew}
	m.mu.Lock()
	m.tasks[id] = t
	m.mu.Unlock()
	return t
}

func (m *Manager) AddItem(id string, rawurl string) (added, limit int, err error) {
	t := m.get(id)
	if t == nil {
		return 0, 3, ErrNotFound
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Started || t.Status == StatusRunning || t.Status == StatusDone {
		return 0, 3, ErrAlreadyStarted
	}
	if len(t.Items) >= 3 {
		return len(t.Items), 3, ErrTooManyItems
	}
	if !isAllowed(rawurl) {
		return len(t.Items), 3, ErrUnsupportedType
	}
	if _, err := url.ParseRequestURI(rawurl); err != nil {
		return len(t.Items), 3, fmt.Errorf("bad url: %w", err)
	}
	t.Items = append(t.Items, rawurl)
	t.Added = len(t.Items)
	return t.Added, 3, nil
}

func (m *Manager) Run(id string, ctx context.Context) error {
	t := m.get(id)
	if t == nil {
		return ErrNotFound
	}
	t.mu.Lock()
	if t.Started {
		t.mu.Unlock()
		return ErrAlreadyStarted
	}
	if len(t.Items) == 0 {
		t.mu.Unlock()
		return ErrNoItems
	}
	t.Started = true
	t.Status = StatusNew
	t.mu.Unlock()

	select {
	case m.sem <- struct{}{}:
	default:
		t.mu.Lock()
		t.Started = false
		t.mu.Unlock()
		return ErrBusy
	}

	go func() {
		defer func() { <-m.sem }()
		m.processTask(t)
	}()
	return nil
}

func (m *Manager) Status(id string) *Task {
	t := m.get(id)
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := *t
	return &out
}

func (m *Manager) get(id string) *Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tasks[id]
}

func (m *Manager) processTask(t *Task) {
	t.mu.Lock()
	t.Status = StatusRunning
	t.Error = ""
	t.Done = 0
	t.mu.Unlock()

	tmpTaskDir := filepath.Join(m.tmpDir, t.ID)
	_ = os.MkdirAll(tmpTaskDir, 0o755)
	defer os.RemoveAll(tmpTaskDir)

	var downloaded []string
	var errs []string

	for _, u := range t.Items {
		fn, err := m.download(u, tmpTaskDir)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", u, err))
		} else {
			downloaded = append(downloaded, fn)
		}
		t.mu.Lock()
		t.Done = len(downloaded)
		t.mu.Unlock()
	}

	if len(downloaded) == 0 {
		t.mu.Lock()
		t.Status = StatusError
		t.Error = "all downloads failed"
		t.mu.Unlock()
		return
	}

	out := filepath.Join(m.outDir, t.ID+".zip")
	if err := zipFiles(out, downloaded); err != nil {
		t.mu.Lock()
		t.Status = StatusError
		t.Error = "zip error: " + err.Error()
		t.mu.Unlock()
		return
	}

	t.mu.Lock()
	t.ResultPath = out
	if len(errs) > 0 {
		t.Status = StatusDone
		t.Error = strings.Join(errs, "; ")
	} else {
		t.Status = StatusDone
	}
	t.mu.Unlock()
}

func (m *Manager) download(u, dir string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}

	limit := m.limitMB * 1024 * 1024
	r := io.LimitReader(resp.Body, limit+1)
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return "", fmt.Errorf("file too large (> %d MB)", m.limitMB)
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	_ = ct

	filename := safeName(filepath.Base(u))
	if filename == "" {
		filename = "file"
	}
	if strings.HasSuffix(strings.ToLower(u), ".pdf") && !strings.HasSuffix(strings.ToLower(filename), ".pdf") {
		filename += ".pdf"
	}
	if strings.HasSuffix(strings.ToLower(u), ".jpeg") && !strings.HasSuffix(strings.ToLower(filename), ".jpeg") {
		filename += ".jpeg"
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func zipFiles(out string, files []string) error {
	tmp := out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for _, p := range files {
		if err := addFile(zw, p); err != nil {
			_ = zw.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	_ = os.Remove(out)
	return os.Rename(tmp, out)
}

func addFile(zw *zip.Writer, path string) error {
	w, err := zw.Create(filepath.Base(path))
	if err != nil {
		return err
	}
	fd, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fd.Close()
	_, err = io.Copy(w, fd)
	return err
}

func isAllowed(u string) bool {
	l := strings.ToLower(u)
	return strings.HasSuffix(l, ".pdf") || strings.HasSuffix(l, ".jpeg")
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func safeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "..", "_")
	if s == "" || s == "." || s == ".." {
		return ""
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	return s
}
