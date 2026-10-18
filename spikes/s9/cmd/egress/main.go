// S9 egress proxy: HTTP CONNECT and plain-HTTP forwarding with a per-run domain allowlist
// (SECURITY.md "Egress proxy"). One JSONL line per request. Listens on TCP (macOS, Windows)
// and optionally a Unix socket (Linux bwrap, where the sandbox has no network namespace route).
//
//	egress -listen 127.0.0.1:18080 [-unix /path/egress.sock] -allow "pypi.org,*.pythonhosted.org" -log out/egress.jsonl
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	token  string // per-run credential; the daemon revokes it when the run ends
	allow  []string
	logMu  sync.Mutex
	logOut io.Writer = os.Stdout
)

func allowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range allow {
		if a == host || (strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:])) {
			return true
		}
	}
	return false
}

func record(m map[string]any) {
	m["ts"] = time.Now().Format(time.RFC3339Nano)
	b, _ := json.Marshal(m)
	logMu.Lock()
	logOut.Write(append(b, '\n'))
	logMu.Unlock()
}

// dialChecked resolves host itself and refuses loopback, private and link-local addresses, so an
// allowlisted name that resolves to 127.0.0.1 can't be used to reach Ollama or noskill (rebinding).
func dialChecked(ctx context.Context, hostport string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsUnspecified() {
			return nil, errors.New("resolves to a local address: " + ip.IP.String())
		}
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, "tcp", net.JoinHostPort(ips[0].IP.String(), port))
}

func handler(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	target := r.Host
	if r.Method != http.MethodConnect {
		target = r.URL.Host
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		host, port = target, "80"
	}
	rec := map[string]any{"method": r.Method, "host": host, "port": port}
	defer func() { rec["ms"] = time.Since(t0).Milliseconds(); record(rec) }()
	if token != "" {
		if _, pw, ok := parseProxyAuth(r.Header.Get("Proxy-Authorization")); !ok || pw != token {
			rec["result"] = "no-credential"
			w.Header().Set("Proxy-Authenticate", `Basic realm="noskill"`)
			http.Error(w, "noskill egress: missing or revoked run credential", http.StatusProxyAuthRequired)
			return
		}
	}
	if net.ParseIP(host) != nil || !allowed(host) {
		rec["result"] = "denied"
		http.Error(w, "noskill egress: "+host+" is not in this run's network allowlist", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		if port != "443" {
			rec["result"] = "denied-port"
			http.Error(w, "noskill egress: only port 443 for CONNECT", http.StatusForbidden)
			return
		}
		up, err := dialChecked(r.Context(), net.JoinHostPort(host, port))
		if err != nil {
			rec["result"], rec["err"] = "dial-failed", err.Error()
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			up.Close()
			return
		}
		cl, buf, err := hj.Hijack()
		if err != nil {
			up.Close()
			return
		}
		cl.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		rec["result"] = "tunnel"
		var wg sync.WaitGroup
		var nUp, nDown int64
		wg.Add(2)
		go func() {
			defer wg.Done()
			if n := buf.Reader.Buffered(); n > 0 {
				p, _ := buf.Reader.Peek(n)
				up.Write(p)
			}
			nUp, _ = io.Copy(up, cl)
			if tc, ok := up.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
		}()
		go func() {
			defer wg.Done()
			nDown, _ = io.Copy(cl, up)
			cl.Close()
		}()
		wg.Wait()
		up.Close()
		rec["bytes_up"], rec["bytes_down"] = nUp, nDown
		return
	}
	// Plain HTTP (absolute-form request line).
	tr := &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return dialChecked(ctx, addr) }}
	r.RequestURI = ""
	r.Header.Del("Proxy-Connection")
	resp, err := tr.RoundTrip(r)
	if err != nil {
		rec["result"], rec["err"] = "dial-failed", err.Error()
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	rec["result"], rec["status"], rec["bytes_down"] = "forward", resp.StatusCode, n
}

func parseProxyAuth(h string) (string, string, bool) {
	r := http.Request{Header: http.Header{"Authorization": {h}}}
	return r.BasicAuth()
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18080", "TCP address")
	unixPath := flag.String("unix", "", "also listen on this Unix socket")
	allowFlag := flag.String("allow", "", "comma-separated hosts; *.example.com for subdomains")
	logPath := flag.String("log", "", "JSONL log file (default stdout)")
	flag.StringVar(&token, "token", "", "require this password in Proxy-Authorization (user ignored)")
	flag.Parse()
	for _, a := range strings.Split(*allowFlag, ",") {
		if a = strings.TrimSpace(strings.ToLower(a)); a != "" {
			allow = append(allow, a)
		}
	}
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatal(err)
		}
		logOut = f
	}
	srv := &http.Server{Handler: http.HandlerFunc(handler), ReadHeaderTimeout: 10 * time.Second}
	if *unixPath != "" {
		os.Remove(*unixPath) // our own socket file from a previous run
		ul, err := net.Listen("unix", *unixPath)
		if err != nil {
			log.Fatal(err)
		}
		go srv.Serve(ul)
	}
	tl, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("egress on %s allow=%v", tl.Addr(), allow)
	log.Fatal(srv.Serve(tl))
}
