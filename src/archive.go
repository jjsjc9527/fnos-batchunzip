package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// ---------------------------------------------------------------------------
// Archive detection
// ---------------------------------------------------------------------------

const (
	roleNone = iota
	roleMain
	roleVolume
)

var (
	rePart1  = regexp.MustCompile(`(?i)^(.+)\.part0*1\.rar$`)
	rePartN  = regexp.MustCompile(`(?i)\.part\d+\.rar$`)
	reNum1   = regexp.MustCompile(`(?i)^(.+)\.(7z|zip|rar|tar)\.0*1$`)
	reNumN   = regexp.MustCompile(`(?i)\.(7z|zip|rar|tar)\.\d+$`)
	reOldVol = regexp.MustCompile(`(?i)\.(r|z)\d{2,3}$`)
	reTar    = regexp.MustCompile(`(?i)^(.+)\.(tar\.gz|tar\.bz2|tar\.xz|tar\.zst|tgz|tbz|tbz2|txz|tzst|tar)$`)
	reSingle = regexp.MustCompile(`(?i)^(.+)\.(zip|7z|rar|gz|bz2|xz)$`)
)

type classInfo struct {
	role  int
	base  string // name without archive extension / volume suffix
	kind  string // "7z" or "tar"
	ext   string // lowercase extension that matched
	multi string // "part" | "num" | "" (volume naming scheme)
}

func classify(name string) classInfo {
	if m := rePart1.FindStringSubmatch(name); m != nil {
		return classInfo{role: roleMain, base: m[1], kind: "7z", ext: "rar", multi: "part"}
	}
	if rePartN.MatchString(name) {
		return classInfo{role: roleVolume}
	}
	if m := reNum1.FindStringSubmatch(name); m != nil {
		return classInfo{role: roleMain, base: m[1], kind: "7z", ext: strings.ToLower(m[2]), multi: "num"}
	}
	if reNumN.MatchString(name) || reOldVol.MatchString(name) {
		return classInfo{role: roleVolume}
	}
	if m := reTar.FindStringSubmatch(name); m != nil {
		return classInfo{role: roleMain, base: m[1], kind: "tar", ext: strings.ToLower(m[2])}
	}
	if m := reSingle.FindStringSubmatch(name); m != nil {
		return classInfo{role: roleMain, base: m[1], kind: "7z", ext: strings.ToLower(m[2])}
	}
	return classInfo{role: roleNone}
}

type Archive struct {
	Path     string   `json:"path"`
	Name     string   `json:"name"`
	Rel      string   `json:"rel,omitempty"`
	Size     int64    `json:"size"`
	MTime    int64    `json:"mtime"`
	VolCount int      `json:"volumes"`
	Kind     string   `json:"kind"`
	Base     string   `json:"-"`
	Ext      string   `json:"-"`
	Volumes  []string `json:"-"`
	Done     bool     `json:"done"`
}

// inspect builds an Archive for the main (first-volume) file at path.
// dirNames is an optional pre-read listing of the containing directory.
func inspect(path string, dirNames []string) (*Archive, error) {
	name := filepath.Base(path)
	ci := classify(name)
	if ci.role != roleMain {
		return nil, fmt.Errorf("不是压缩包（或不是第一个分卷）: %s", name)
	}
	dir := filepath.Dir(path)
	if dirNames == nil {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			dirNames = append(dirNames, e.Name())
		}
	}
	var re *regexp.Regexp
	q := regexp.QuoteMeta(ci.base)
	switch {
	case ci.multi == "part":
		re = regexp.MustCompile(`(?i)^` + q + `\.part\d+\.rar$`)
	case ci.multi == "num":
		re = regexp.MustCompile(`(?i)^` + q + `\.` + ci.ext + `\.\d+$`)
	case ci.ext == "rar":
		re = regexp.MustCompile(`(?i)^` + q + `\.(rar|r\d{2,3})$`)
	case ci.ext == "zip":
		re = regexp.MustCompile(`(?i)^` + q + `\.(zip|z\d{2})$`)
	}
	vols := []string{path}
	if re != nil {
		vols = nil
		for _, n := range dirNames {
			if re.MatchString(n) {
				vols = append(vols, filepath.Join(dir, n))
			}
		}
		if len(vols) == 0 {
			vols = []string{path}
		}
		sort.Strings(vols)
	}
	a := &Archive{Path: path, Name: name, Kind: ci.kind, Base: ci.base, Ext: ci.ext, Volumes: vols, VolCount: len(vols)}
	for _, v := range vols {
		st, err := os.Stat(v)
		if err != nil {
			continue
		}
		a.Size += st.Size()
		if t := st.ModTime().Unix(); t > a.MTime {
			a.MTime = t
		}
	}
	return a, nil
}

func (a *Archive) key() string {
	st, err := os.Stat(a.Path)
	if err != nil {
		return a.Path
	}
	return fmt.Sprintf("%s|%d|%d", a.Path, st.Size(), st.ModTime().Unix())
}

// skipDir reports directories that should never be scanned.
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#")
}

// ---------------------------------------------------------------------------
// Extraction
// ---------------------------------------------------------------------------

type ExtractOpts struct {
	DestParent string
	Smart      bool
	Passwords  []string
	Progress   func(pct int, note string)
}

var errWrongPassword = errors.New("wrong password")

var rePct = regexp.MustCompile(`(\d{1,3})%`)

// run7z runs one 7-Zip extraction attempt.
func run7z(ctx context.Context, archive, outDir, pw string, progress func(int, string)) error {
	cmd := exec.CommandContext(ctx, sevenZip, "x", "-y", "-bso0", "-bse2", "-bsp1", "-p"+pw, "-o"+outDir, "--", archive)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("无法启动 7-Zip: %v", err)
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sc.Split(splitProgress)
	last := -1
	for sc.Scan() {
		if m := rePct.FindStringSubmatch(sc.Text()); m != nil {
			if p, _ := strconv.Atoi(m[1]); p != last && p <= 100 {
				last = p
				progress(p, "")
			}
		}
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		msg := stderr.String()
		if strings.Contains(strings.ToLower(msg), "wrong password") {
			return errWrongPassword
		}
		return errors.New(explain(msg))
	}
	return nil
}

// splitProgress splits 7-Zip progress output on \b, \r and \n.
func splitProgress(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\b' || b == '\r' || b == '\n' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

type countingReader struct {
	r     io.Reader
	n     int64
	total int64
	cb    func(int, string)
	last  int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.total > 0 {
		pct := int(c.n * 100 / c.total)
		if pct != c.last {
			c.last = pct
			c.cb(pct, "")
		}
	}
	return n, err
}

func runTar(ctx context.Context, a *Archive, outDir string, progress func(int, string)) error {
	flag := ""
	switch a.Ext {
	case "tar.gz", "tgz":
		flag = "-z"
	case "tar.bz2", "tbz", "tbz2":
		flag = "-j"
	case "tar.xz", "txz":
		flag = "-J"
	case "tar.zst", "tzst":
		flag = "--zstd"
	}
	f, err := os.Open(a.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, _ := f.Stat()
	args := []string{"-x"}
	if flag != "" {
		args = append(args, flag)
	}
	args = append(args, "-f", "-", "-C", outDir, "--no-same-owner")
	cmd := exec.CommandContext(ctx, "tar", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Stdin = &countingReader{r: f, total: st.Size(), cb: progress, last: -1}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return errors.New(explain(stderr.String()))
	}
	return nil
}

// explain turns tool error output into a readable Chinese message.
func explain(o string) string {
	l := strings.ToLower(o)
	switch {
	case strings.Contains(l, "no space") || strings.Contains(l, "disk full"):
		return "磁盘空间不足"
	case strings.Contains(l, "permission denied"):
		return "没有权限写入目标文件夹（请在 应用设置 里给本应用授权该文件夹的读写权限）"
	case strings.Contains(l, "missing volume") || strings.Contains(l, "unexpected end") ||
		strings.Contains(l, "unavailable") || strings.Contains(l, "there are some data after"):
		return "分卷不全或文件没下载完整"
	case strings.Contains(l, "crc failed") || strings.Contains(l, "data error") || strings.Contains(l, "headers error"):
		return "数据校验失败，压缩包已损坏"
	case strings.Contains(l, "cannot open the file as") || strings.Contains(l, "is not archive") ||
		strings.Contains(l, "not in gzip format") || strings.Contains(l, "does not look like a tar"):
		return "不是有效的压缩包或已损坏"
	case strings.Contains(l, "zstd") && strings.Contains(l, "not found"):
		return "系统缺少 zstd，无法解压 .zst"
	}
	for _, line := range strings.Split(o, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "ERROR: /") {
			continue
		}
		if strings.Contains(strings.ToLower(line), "error") || strings.Contains(strings.ToLower(line), "cannot") {
			return line
		}
	}
	if s := strings.TrimSpace(o); s != "" {
		if len(s) > 200 {
			s = s[:200]
		}
		return s
	}
	return "解压失败（原因未知）"
}

// extract extracts a into opts.DestParent and returns the final path.
func extract(ctx context.Context, a *Archive, opts ExtractOpts) (string, string, error) {
	if err := os.MkdirAll(opts.DestParent, 0o777); err != nil {
		return "", "", errors.New(explain(err.Error()))
	}
	tmp, err := os.MkdirTemp(opts.DestParent, ".unpacking-")
	if err != nil {
		return "", "", errors.New(explain(err.Error()))
	}
	cleanup := func() { os.RemoveAll(tmp) }
	usedPw := ""

	if a.Kind == "tar" {
		if err := runTar(ctx, a, tmp, opts.Progress); err != nil {
			cleanup()
			return "", "", err
		}
	} else {
		pws := append([]string{""}, opts.Passwords...)
		var lastErr error
		ok := false
		for i, pw := range pws {
			if i > 0 {
				opts.Progress(0, fmt.Sprintf("尝试第 %d 个密码", i))
				os.RemoveAll(tmp)
				os.MkdirAll(tmp, 0o777)
			}
			lastErr = run7z(ctx, a.Path, tmp, pw, opts.Progress)
			if lastErr == nil {
				ok = true
				usedPw = pw
				break
			}
			if lastErr != errWrongPassword {
				break
			}
		}
		if !ok {
			cleanup()
			if lastErr == errWrongPassword {
				if len(opts.Passwords) == 0 {
					return "", "", errors.New("压缩包有密码：请在“密码本”里添加密码，或解压时填写密码")
				}
				return "", "", errors.New("密码不对：密码本里的密码都试过了")
			}
			return "", "", lastErr
		}
	}

	fixNames(tmp)
	makeAccessible(tmp)

	ents, _ := os.ReadDir(tmp)
	var final string
	switch {
	case len(ents) == 0:
		cleanup()
		return "", usedPw, errors.New("压缩包是空的")
	case opts.Smart && len(ents) == 1:
		final = uniquePath(filepath.Join(opts.DestParent, ents[0].Name()))
		if err := os.Rename(filepath.Join(tmp, ents[0].Name()), final); err != nil {
			cleanup()
			return "", usedPw, err
		}
		os.Remove(tmp)
	default:
		final = uniquePath(filepath.Join(opts.DestParent, a.Base))
		if err := os.Rename(tmp, final); err != nil {
			cleanup()
			return "", usedPw, err
		}
		os.Chmod(final, 0o777)
	}
	return final, usedPw, nil
}

// uniquePath returns p, or "p (1)", "p (2)"... if p exists. Keeps file extensions.
func uniquePath(p string) string {
	if _, err := os.Lstat(p); err != nil {
		return p
	}
	dir, base := filepath.Dir(p), filepath.Base(p)
	ext := ""
	if st, err := os.Lstat(p); err == nil && !st.IsDir() {
		if i := strings.LastIndex(base, "."); i > 0 {
			ext = base[i:]
			base = base[:i]
		}
	}
	for i := 1; ; i++ {
		c := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Lstat(c); err != nil {
			return c
		}
	}
}

// fixNames renames entries whose names are not valid UTF-8 (usually GBK from
// Windows-made zips) to UTF-8.
func fixNames(root string) {
	var paths []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != root {
			paths = append(paths, p)
		}
		return nil
	})
	// deepest first so parent renames don't invalidate child paths
	sort.Slice(paths, func(i, j int) bool {
		return strings.Count(paths[i], "/") > strings.Count(paths[j], "/")
	})
	dec := simplifiedchinese.GB18030.NewDecoder()
	for _, p := range paths {
		name := filepath.Base(p)
		if utf8.ValidString(name) {
			continue
		}
		n, err := dec.String(name)
		if err != nil || n == "" || !utf8.ValidString(n) {
			continue
		}
		os.Rename(p, uniquePath(filepath.Join(filepath.Dir(p), n)))
	}
}

// makeAccessible gives everyone read/write so the NAS user who owns the folder
// can manage files written by the app's own system user.
func makeAccessible(root string) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if d.IsDir() {
			os.Chmod(p, info.Mode().Perm()|0o777)
		} else {
			os.Chmod(p, info.Mode().Perm()|0o666)
		}
		return nil
	})
}

// moveFile renames, falling back to copy+delete across filesystems.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}
