package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

// fnOS same-origin CGI gateway.
//
// fnOS serves /cgi/ThirdParty/<appname>/index.cgi/<rest> on its own web port
// (5666/5667) and runs ui/index.cgi for every request. That is the only route
// that works through FN Connect / the mobile app, because remote access only
// forwards fnOS's own port. index.cgi execs this binary with -cgi, which
// forwards the request to the daemon on 127.0.0.1 and streams the response
// back unchanged.

const cgiName = "/index.cgi"

func runCGI(port string) {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", port)}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			prefix, rest := splitCGIPath(pr.In)
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = rest
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target.Host
			pr.Out.Header.Set("X-Forwarded-Prefix", prefix)
			if ip := strings.TrimSpace(os.Getenv("REMOTE_ADDR")); ip != "" {
				pr.Out.Header.Set("X-Forwarded-For", ip)
			}
		},
		Transport: &http.Transport{
			Proxy:                 nil, // always talk to loopback directly
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			ResponseHeaderTimeout: 60 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">`+
				`<meta name="viewport" content="width=device-width,initial-scale=1"><title>批量解压</title></head>`+
				`<body style="font-family:-apple-system,'PingFang SC','Microsoft YaHei',sans-serif;padding:32px 20px;color:#334155;line-height:1.7">`+
				`<h2 style="margin:0 0 8px">批量解压没有在运行</h2>`+
				`<p>请到飞牛「应用中心 → 已安装」里启动「批量解压」，然后刷新本页面。</p></body></html>`)
		},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix, rest := splitCGIPath(r)
		// Without the trailing slash relative URLs (api/…, icon.png) would resolve
		// one level too high.
		if rest == "/" && prefix != "" && !strings.HasSuffix(r.URL.Path, "/") {
			loc := prefix + "/"
			if r.URL.RawQuery != "" {
				loc += "?" + r.URL.RawQuery
			}
			w.Header().Set("Location", loc)
			w.WriteHeader(http.StatusFound)
			return
		}
		proxy.ServeHTTP(w, r)
	})

	if err := cgi.Serve(handler); err != nil {
		fmt.Print("Status: 500 Internal Server Error\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
		fmt.Println("CGI 出错:", err)
	}
}

// splitCGIPath returns the public prefix (".../index.cgi") and the path that
// should be forwarded to the daemon ("/api/state").
func splitCGIPath(r *http.Request) (prefix, rest string) {
	p := r.URL.Path
	if i := strings.Index(p, cgiName); i >= 0 {
		prefix = p[:i+len(cgiName)]
		rest = p[i+len(cgiName):]
	} else {
		rest = os.Getenv("PATH_INFO")
		if sn := os.Getenv("SCRIPT_NAME"); strings.HasSuffix(sn, cgiName) {
			prefix = sn
		}
	}
	if rest == "" {
		rest = "/"
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return prefix, rest
}
