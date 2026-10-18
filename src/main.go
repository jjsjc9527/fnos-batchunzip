package main

import (
	"embed"
	"encoding/json"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

var (
	sevenZip   string
	dataDir    string
	extraRoots []string
	version    = "dev"
)

func main() {
	port := flag.String("port", envOr("TRIM_SERVICE_PORT", "18660"), "listen port")
	flag.StringVar(&dataDir, "data", envOr("TRIM_PKGVAR", "./data"), "data directory")
	root := flag.String("root", "", "extra browse roots for local testing (colon separated)")
	cgiMode := flag.Bool("cgi", false, "run as the fnOS CGI gateway (used by ui/index.cgi)")
	flag.Parse()

	if *cgiMode {
		runCGI(envOr("BU_PORT", "18660"))
		return
	}

	exe, _ := os.Executable()
	appDir := envOr("TRIM_APPDEST", filepath.Dir(filepath.Dir(exe)))
	for _, c := range []string{filepath.Join(appDir, "bin", "7zz"), filepath.Join(filepath.Dir(exe), "7zz")} {
		if _, err := os.Stat(c); err == nil {
			sevenZip = c
			break
		}
	}
	if sevenZip == "" {
		sevenZip = "7zz"
	}
	if *root != "" {
		extraRoots = strings.Split(*root, ":")
	}

	// Files we create should be manageable by the NAS user, not only by the app user.
	syscall.Umask(0)

	st := openStore(dataDir)
	mgr := newManager(st)
	w := newWatcher(mgr, st)
	srv := &server{st: st, mgr: mgr, w: w}

	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", noCache(http.FileServer(http.FS(sub))))
	mux.HandleFunc("GET /api/state", srv.state)
	mux.HandleFunc("POST /api/setup", srv.setup)
	mux.HandleFunc("POST /api/login", srv.login)
	mux.HandleFunc("POST /api/logout", srv.auth(srv.logout))
	mux.HandleFunc("GET /api/roots", srv.auth(srv.roots))
	mux.HandleFunc("GET /api/list", srv.auth(srv.list))
	mux.HandleFunc("GET /api/settings", srv.auth(srv.getSettings))
	mux.HandleFunc("POST /api/settings", srv.auth(srv.saveSettings))
	mux.HandleFunc("POST /api/password", srv.auth(srv.changePassword))
	mux.HandleFunc("POST /api/done/clear", srv.auth(srv.clearDone))
	mux.HandleFunc("GET /api/jobs", srv.auth(srv.jobs))
	mux.HandleFunc("POST /api/jobs", srv.auth(srv.submit))
	mux.HandleFunc("POST /api/jobs/{id}/cancel", srv.auth(srv.cancelJob))
	mux.HandleFunc("POST /api/jobs/clear", srv.auth(srv.clearJobs))
	mux.HandleFunc("POST /api/watch/run", srv.auth(srv.watchRun))

	addr := ":" + *port
	log.Printf("批量解压 %s 启动，端口 %s，7-Zip: %s，数据目录: %s", version, *port, sevenZip, dataDir)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

type server struct {
	st  *Store
	mgr *Manager
	w   *Watcher
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v)
}

const (
	cookieName  = "bu_session"
	tokenHeader = "X-BU-Token"
)

// sessionToken reads the session from the cookie or, when a webview / gateway
// drops cookies, from the X-BU-Token header the page sends as well.
func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return r.Header.Get(tokenHeader)
}

func (s *server) loggedIn(r *http.Request) bool {
	if s.st.ValidSession(sessionToken(r)) {
		return true
	}
	// a stale cookie must not hide a valid header token
	return s.st.ValidSession(r.Header.Get(tokenHeader))
}

func (s *server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.loggedIn(r) {
			fail(w, 401, "请先登录")
			return
		}
		h(w, r)
	}
}

// cookiePath scopes the cookie to the CGI prefix when served through fnOS, so
// it is not sent with every other fnOS request.
func cookiePath(r *http.Request) string {
	p := r.Header.Get("X-Forwarded-Prefix")
	if strings.HasPrefix(p, "/cgi/") && !strings.ContainsAny(p, ";,\r\n \"") {
		return p + "/"
	}
	return "/"
}

func (s *server) setCookie(w http.ResponseWriter, r *http.Request, tok string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: cookiePath(r), HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

func (s *server) startSession(w http.ResponseWriter, r *http.Request) {
	tok := s.st.NewSession()
	s.setCookie(w, r, tok, int(sessionTTL.Seconds()))
	reply(w, map[string]any{"ok": true, "token": tok})
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	reply(w, map[string]any{"needSetup": s.st.NeedSetup(), "loggedIn": s.loggedIn(r), "version": version})
}

var (
	loginMu    sync.Mutex
	loginFails = map[string][]time.Time{}
)

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		h = r.RemoteAddr
	}
	// requests through the fnOS CGI gateway come from loopback
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		if xff := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); xff != "" {
			return xff
		}
	}
	return h
}

func (s *server) setup(w http.ResponseWriter, r *http.Request) {
	var b struct{ Password string }
	if decode(r, &b) != nil || len(b.Password) < 4 {
		fail(w, 400, "密码至少 4 位")
		return
	}
	if !s.st.NeedSetup() {
		fail(w, 400, "已经设置过访问密码了")
		return
	}
	s.st.SetPassword(b.Password)
	s.startSession(w, r)
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	loginMu.Lock()
	defer loginMu.Unlock()
	var recent []time.Time
	for _, t := range loginFails[ip] {
		if time.Since(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= 10 {
		fail(w, 429, "密码错误次数太多，请 10 分钟后再试")
		return
	}
	var b struct{ Password string }
	decode(r, &b)
	if !s.st.CheckPassword(b.Password) {
		loginFails[ip] = append(recent, time.Now())
		fail(w, 403, "密码不对")
		return
	}
	delete(loginFails, ip)
	s.startSession(w, r)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	s.st.DropSession(sessionToken(r))
	s.st.DropSession(r.Header.Get(tokenHeader))
	s.setCookie(w, r, "", -1)
	reply(w, map[string]bool{"ok": true})
}

func (s *server) changePassword(w http.ResponseWriter, r *http.Request) {
	var b struct{ Old, New string }
	decode(r, &b)
	if !s.st.CheckPassword(b.Old) {
		fail(w, 403, "原密码不对")
		return
	}
	if len(b.New) < 4 {
		fail(w, 400, "新密码至少 4 位")
		return
	}
	s.st.SetPassword(b.New)
	s.startSession(w, r)
}

// accessibleRoots returns folders the app has been authorized to use.
func accessibleRoots() []string {
	var raw []string
	if b, err := os.ReadFile(filepath.Join(dataDir, "accessible_paths")); err == nil {
		raw = append(raw, strings.Split(strings.TrimSpace(string(b)), ":")...)
	} else {
		raw = append(raw, strings.Split(os.Getenv("TRIM_DATA_ACCESSIBLE_PATHS"), ":")...)
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "share_paths")); err == nil {
		raw = append(raw, strings.Split(strings.TrimSpace(string(b)), ":")...)
	} else {
		raw = append(raw, strings.Split(os.Getenv("TRIM_DATA_SHARE_PATHS"), ":")...)
	}
	raw = append(raw, extraRoots...)
	seen := map[string]bool{}
	var out []string
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

func (s *server) roots(w http.ResponseWriter, r *http.Request) {
	type root struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	var out []root
	for _, p := range accessibleRoots() {
		out = append(out, root{p, filepath.Base(p)})
	}
	if out == nil {
		out = []root{}
	}
	reply(w, out)
}

type dirEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	MTime int64  `json:"mtime"`
}

func cleanAbs(p string) (string, bool) {
	if p == "" || !filepath.IsAbs(p) {
		return "", false
	}
	return filepath.Clean(p), true
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	p, ok := cleanAbs(r.URL.Query().Get("path"))
	if !ok {
		fail(w, 400, "路径不对")
		return
	}
	recursive := r.URL.Query().Get("recursive") == "1"
	st, err := os.Stat(p)
	if err != nil {
		if os.IsPermission(err) {
			fail(w, 403, "没有权限访问这个文件夹。请到 飞牛「设置 → 应用设置 → 批量解压」里授权这个文件夹。")
		} else {
			fail(w, 404, "文件夹不存在")
		}
		return
	}
	if !st.IsDir() {
		p = filepath.Dir(p)
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		fail(w, 403, "没有权限读取这个文件夹。请到 飞牛「设置 → 应用设置 → 批量解压」里授权这个文件夹。")
		return
	}
	dirs := []dirEntry{}
	archives := []*Archive{}
	others := 0
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	for _, e := range ents {
		full := filepath.Join(p, e.Name())
		if e.IsDir() {
			if skipDir(e.Name()) {
				continue
			}
			var mt int64
			if info, err := e.Info(); err == nil {
				mt = info.ModTime().Unix()
			}
			dirs = append(dirs, dirEntry{e.Name(), full, mt})
			continue
		}
		if recursive {
			continue
		}
		switch classify(e.Name()).role {
		case roleMain:
			if a, err := inspect(full, names); err == nil {
				a.Done = s.st.IsDone(a.key())
				archives = append(archives, a)
			}
		case roleVolume:
		default:
			others++
		}
	}
	truncated := false
	if recursive {
		filepath.WalkDir(p, func(fp string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if fp != p && skipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if len(archives) >= 5000 {
				truncated = true
				return filepath.SkipAll
			}
			if classify(d.Name()).role == roleMain {
				if a, err := inspect(fp, nil); err == nil {
					a.Done = s.st.IsDone(a.key())
					a.Rel, _ = filepath.Rel(p, filepath.Dir(fp))
					if a.Rel == "." {
						a.Rel = ""
					}
					archives = append(archives, a)
				}
			}
			return nil
		})
	}
	sort.Slice(dirs, func(i, j int) bool { return naturalLess(dirs[i].Name, dirs[j].Name) })
	sort.Slice(archives, func(i, j int) bool {
		if archives[i].Rel != archives[j].Rel {
			return naturalLess(archives[i].Rel, archives[j].Rel)
		}
		return naturalLess(archives[i].Name, archives[j].Name)
	})
	parent := filepath.Dir(p)
	if parent == p {
		parent = ""
	}
	reply(w, map[string]any{"path": p, "parent": parent, "dirs": dirs, "archives": archives,
		"others": others, "truncated": truncated})
}

// naturalLess compares strings so that "第2集" < "第10集".
func naturalLess(a, b string) bool {
	ra, rb := []rune(strings.ToLower(a)), []rune(strings.ToLower(b))
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		ca, cb := ra[i], rb[j]
		if isDigit(ca) && isDigit(cb) {
			si := i
			for i < len(ra) && isDigit(ra[i]) {
				i++
			}
			sj := j
			for j < len(rb) && isDigit(rb[j]) {
				j++
			}
			na, _ := strconv.ParseInt(string(ra[si:i]), 10, 64)
			nb, _ := strconv.ParseInt(string(rb[sj:j]), 10, 64)
			if na != nb {
				return na < nb
			}
			continue
		}
		if ca != cb {
			return ca < cb
		}
		i++
		j++
	}
	return len(ra)-i < len(rb)-j
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func (s *server) getSettings(w http.ResponseWriter, r *http.Request) {
	st := s.st.Get()
	last, msg := s.w.Status()
	var lastUnix int64
	if !last.IsZero() {
		lastUnix = last.Unix()
	}
	reply(w, map[string]any{"passwords": st.Passwords, "defaults": st.Defaults, "watch": st.Watch,
		"watchLast": lastUnix, "watchMsg": msg, "sevenZip": sevenZip, "version": version})
}

func (s *server) saveSettings(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Passwords *[]string      `json:"passwords"`
		Defaults  *JobOptions    `json:"defaults"`
		Watch     *WatchSettings `json:"watch"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, "数据格式不对")
		return
	}
	s.st.Update(func(st *Settings) {
		if b.Passwords != nil {
			var pws []string
			seen := map[string]bool{}
			for _, p := range *b.Passwords {
				p = strings.TrimRight(p, "\r")
				if p != "" && !seen[p] {
					seen[p] = true
					pws = append(pws, p)
				}
			}
			st.Passwords = pws
		}
		if b.Defaults != nil {
			b.Defaults.Extra = nil
			st.Defaults = *b.Defaults
		}
		if b.Watch != nil {
			b.Watch.Opts.Extra = nil
			if b.Watch.IntervalMin < 1 {
				b.Watch.IntervalMin = 1
			}
			st.Watch = *b.Watch
		}
	})
	reply(w, map[string]bool{"ok": true})
}

func (s *server) clearDone(w http.ResponseWriter, r *http.Request) {
	s.st.ClearDone()
	reply(w, map[string]bool{"ok": true})
}

func (s *server) jobs(w http.ResponseWriter, r *http.Request) {
	reply(w, s.mgr.Snapshot())
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Files []string   `json:"files"`
		Opts  JobOptions `json:"opts"`
	}
	if err := decode(r, &b); err != nil || len(b.Files) == 0 {
		fail(w, 400, "没有选择压缩包")
		return
	}
	var files []string
	for _, f := range b.Files {
		if p, ok := cleanAbs(f); ok {
			files = append(files, p)
		}
	}
	if b.Opts.OutMode == "dir" && b.Opts.OutDir == "" {
		fail(w, 400, "请选择解压到哪个文件夹")
		return
	}
	if b.Opts.After == "move" && b.Opts.MoveTo == "" {
		fail(w, 400, "请选择压缩包要移动到哪个文件夹")
		return
	}
	title := "解压 " + strconv.Itoa(len(files)) + " 个压缩包"
	if len(files) == 1 {
		title = "解压 " + filepath.Base(files[0])
	}
	j := s.mgr.Submit(title, false, files, b.Opts)
	if len(j.Tasks) == 0 {
		fail(w, 400, "这些压缩包已经在任务队列里了")
		return
	}
	reply(w, map[string]int{"id": j.ID, "count": len(j.Tasks)})
}

func (s *server) cancelJob(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.mgr.Cancel(id)
	reply(w, map[string]bool{"ok": true})
}

func (s *server) clearJobs(w http.ResponseWriter, r *http.Request) {
	s.mgr.ClearFinished()
	reply(w, map[string]bool{"ok": true})
}

func (s *server) watchRun(w http.ResponseWriter, r *http.Request) {
	s.w.RunNow()
	reply(w, map[string]bool{"ok": true})
}
