package main

import (
	"io/fs"
	"log"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Watcher periodically scans configured folders and queues new archives.
type Watcher struct {
	mgr     *Manager
	store   *Store
	mu      sync.Mutex
	lastRun time.Time
	LastMsg string
	failed  map[string]bool // archive keys that failed; retried only when the file changes
	kick    chan struct{}
}

func newWatcher(m *Manager, st *Store) *Watcher {
	w := &Watcher{mgr: m, store: st, failed: map[string]bool{}, kick: make(chan struct{}, 1)}
	go w.loop()
	return w
}

func (w *Watcher) RunNow() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *Watcher) Status() (time.Time, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastRun, w.LastMsg
}

func (w *Watcher) loop() {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		force := false
		select {
		case <-tick.C:
		case <-w.kick:
			force = true
		}
		s := w.store.Get().Watch
		if !s.Enabled || len(s.Dirs) == 0 {
			continue
		}
		iv := time.Duration(max(s.IntervalMin, 1)) * time.Minute
		w.mu.Lock()
		due := force || time.Since(w.lastRun) >= iv
		w.mu.Unlock()
		if due {
			w.scan(s)
		}
	}
}

// collectFailures remembers archives whose auto job failed, so they are not
// retried every scan.
func (w *Watcher) noteFailures() {
	for _, j := range w.mgr.Snapshot() {
		if !j.Auto {
			continue
		}
		for _, t := range j.Tasks {
			if t.Status == "fail" {
				if a, err := inspect(t.Path, nil); err == nil {
					w.failed[a.key()] = true
				}
			}
		}
	}
}

func (w *Watcher) scan(s WatchSettings) {
	w.mu.Lock()
	w.lastRun = time.Now()
	w.mu.Unlock()
	w.noteFailures()

	stable := time.Duration(max(s.StableMin, 0)) * time.Minute
	var found []string
	waiting := 0
	for _, root := range s.Dirs {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != root && skipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if classify(d.Name()).role != roleMain {
				return nil
			}
			a, err := inspect(p, nil)
			if err != nil {
				return nil
			}
			k := a.key()
			if w.store.IsDone(k) || w.failed[k] || w.mgr.IsQueued(p) {
				return nil
			}
			if time.Since(time.Unix(a.MTime, 0)) < stable {
				waiting++
				return nil
			}
			found = append(found, p)
			return nil
		})
	}
	msg := ""
	switch {
	case len(found) > 0:
		opts := s.Opts
		opts.SkipDone = true
		w.mgr.Submit("自动解压", true, found, opts)
		msg = "发现 " + strconv.Itoa(len(found)) + " 个新压缩包，已加入任务"
	case waiting > 0:
		msg = strconv.Itoa(waiting) + " 个压缩包还在下载/复制中，稍后再处理"
	default:
		msg = "没有新的压缩包"
	}
	if waiting > 0 && len(found) > 0 {
		msg += "；另有 " + strconv.Itoa(waiting) + " 个还在变化中"
	}
	w.mu.Lock()
	w.LastMsg = msg
	w.mu.Unlock()
	log.Printf("watch: %s", msg)
}
