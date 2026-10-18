package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Task struct {
	ID       int    `json:"id"`
	Path     string `json:"path"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Status   string `json:"status"` // queued | running | ok | fail | skip | cancel
	Progress int    `json:"progress"`
	Note     string `json:"note"`
	Message  string `json:"message"`
	Result   string `json:"result"`
	Started  int64  `json:"started"`
	Ended    int64  `json:"ended"`
}

type Job struct {
	ID        int        `json:"id"`
	Title     string     `json:"title"`
	Auto      bool       `json:"auto"`
	Created   int64      `json:"created"`
	Opts      JobOptions `json:"opts"`
	Tasks     []*Task    `json:"tasks"`
	Cancelled bool       `json:"cancelled"`
}

type queued struct {
	job  *Job
	task *Task
}

type Manager struct {
	mu      sync.Mutex
	store   *Store
	jobs    []*Job
	queue   []queued
	nextID  int
	wake    chan struct{}
	cancel  context.CancelFunc
	running *Task
	inQueue map[string]bool // archive paths queued or running
}

func newManager(st *Store) *Manager {
	m := &Manager{store: st, wake: make(chan struct{}, 1), inQueue: map[string]bool{}}
	go m.loop()
	return m
}

func (m *Manager) id() int { m.nextID++; return m.nextID }

// Submit creates a job for the given archive paths.
func (m *Manager) Submit(title string, auto bool, paths []string, opts JobOptions) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := &Job{ID: m.id(), Title: title, Auto: auto, Created: time.Now().Unix(), Opts: opts}
	for _, p := range paths {
		if m.inQueue[p] {
			continue
		}
		t := &Task{ID: m.id(), Path: p, Name: filepath.Base(p), Status: "queued"}
		if st, err := os.Stat(p); err == nil {
			t.Size = st.Size()
		}
		j.Tasks = append(j.Tasks, t)
		m.queue = append(m.queue, queued{j, t})
		m.inQueue[p] = true
	}
	if len(j.Tasks) == 0 {
		return j
	}
	m.jobs = append([]*Job{j}, m.jobs...)
	m.trimLocked()
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return j
}

func (m *Manager) trimLocked() {
	if len(m.jobs) <= 60 {
		return
	}
	keep := m.jobs[:0]
	for i, j := range m.jobs {
		if i < 60 || !jobFinished(j) {
			keep = append(keep, j)
		}
	}
	m.jobs = keep
}

func jobFinished(j *Job) bool {
	for _, t := range j.Tasks {
		if t.Status == "queued" || t.Status == "running" {
			return false
		}
	}
	return true
}

func (m *Manager) Cancel(jobID int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.ID != jobID {
			continue
		}
		j.Cancelled = true
		for _, t := range j.Tasks {
			if t.Status == "queued" {
				t.Status = "cancel"
				t.Message = "已取消"
				delete(m.inQueue, t.Path)
			}
		}
		if m.running != nil {
			for _, t := range j.Tasks {
				if t == m.running && m.cancel != nil {
					m.cancel()
				}
			}
		}
	}
}

func (m *Manager) ClearFinished() {
	m.mu.Lock()
	defer m.mu.Unlock()
	keep := m.jobs[:0]
	for _, j := range m.jobs {
		if !jobFinished(j) {
			keep = append(keep, j)
		}
	}
	m.jobs = keep
}

// Snapshot returns a deep copy safe to serialize.
func (m *Manager) Snapshot() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		c := *j
		c.Opts.Extra = nil
		c.Tasks = make([]*Task, len(j.Tasks))
		for i, t := range j.Tasks {
			tc := *t
			c.Tasks[i] = &tc
		}
		out = append(out, c)
	}
	return out
}

func (m *Manager) IsQueued(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inQueue[path]
}

func (m *Manager) next() (queued, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.queue) > 0 {
		q := m.queue[0]
		m.queue = m.queue[1:]
		if q.task.Status == "queued" {
			return q, true
		}
	}
	return queued{}, false
}

func (m *Manager) loop() {
	for {
		q, ok := m.next()
		if !ok {
			<-m.wake
			continue
		}
		m.runTask(q.job, q.task)
	}
}

func (m *Manager) set(fn func()) {
	m.mu.Lock()
	fn()
	m.mu.Unlock()
}

func (m *Manager) runTask(j *Job, t *Task) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.set(func() {
		t.Status, t.Started, t.Progress = "running", time.Now().Unix(), 0
		m.running, m.cancel = t, cancel
	})
	finish := func(status, msg, result string) {
		m.set(func() {
			t.Status, t.Message, t.Result, t.Ended, t.Note = status, msg, result, time.Now().Unix(), ""
			if status == "ok" {
				t.Progress = 100
			}
			m.running, m.cancel = nil, nil
			delete(m.inQueue, t.Path)
		})
		log.Printf("[%s] %s %s %s", status, t.Path, msg, result)
	}

	a, err := inspect(t.Path, nil)
	if err != nil {
		finish("fail", err.Error(), "")
		return
	}
	key := a.key()
	if j.Opts.SkipDone && m.store.IsDone(key) {
		finish("skip", "以前已经解压过（可在选项里关闭“跳过已解压过的”）", "")
		return
	}

	dest := filepath.Dir(a.Path)
	if j.Opts.OutMode == "dir" && strings.TrimSpace(j.Opts.OutDir) != "" {
		dest = filepath.Clean(j.Opts.OutDir)
	}
	pws := append(append([]string(nil), j.Opts.Extra...), m.store.Get().Passwords...)

	final, usedPw, err := extract(ctx, a, ExtractOpts{
		DestParent: dest,
		Smart:      j.Opts.Smart,
		Passwords:  pws,
		Progress: func(p int, note string) {
			m.set(func() {
				if p >= 0 {
					t.Progress = p
				}
				if note != "" {
					t.Note = note
				}
			})
		},
	})
	if ctx.Err() != nil {
		finish("cancel", "已取消", "")
		return
	}
	if err != nil {
		finish("fail", err.Error(), "")
		return
	}
	m.store.MarkDone(key)

	msg := ""
	if usedPw != "" {
		msg = "密码：" + usedPw
	}
	switch j.Opts.After {
	case "delete":
		var bad []string
		for _, v := range a.Volumes {
			if err := os.Remove(v); err != nil {
				bad = append(bad, filepath.Base(v))
			}
		}
		msg = joinMsg(msg, "已删除压缩包")
		if len(bad) > 0 {
			msg = joinMsg(msg, "但这些没删掉："+strings.Join(bad, "、"))
		}
	case "move":
		if j.Opts.MoveTo != "" {
			os.MkdirAll(j.Opts.MoveTo, 0o777)
			moved := 0
			for _, v := range a.Volumes {
				if err := moveFile(v, uniquePath(filepath.Join(j.Opts.MoveTo, filepath.Base(v)))); err == nil {
					moved++
				}
			}
			msg = joinMsg(msg, fmt.Sprintf("已把压缩包移到 %s", j.Opts.MoveTo))
			if moved < len(a.Volumes) {
				msg = joinMsg(msg, fmt.Sprintf("（%d 个没移动成功）", len(a.Volumes)-moved))
			}
		}
	}
	finish("ok", msg, final)
}

func joinMsg(a, b string) string {
	if a == "" {
		return b
	}
	return a + "；" + b
}
