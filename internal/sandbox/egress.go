package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"noskill/internal/perms"
)

// PublicAddr reports whether a is a public unicast address. Everything else (loopback, private,
// link-local, CGNAT, unspecified, multicast) could reach Ollama, noskill or the LAN.
func PublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	switch {
	case !a.IsValid(), a.IsLoopback(), a.IsPrivate(), a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(),
		a.IsInterfaceLocalMulticast(), a.IsMulticast(), a.IsUnspecified():
		return false
	}
	for _, p := range blocked {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can embed a private v4 address
}

// PublicDialer resolves names itself and connects only to public addresses, by IP, so a name can't
// re-resolve to 127.0.0.1 between the check and the connection (DNS rebinding). Built-in tools
// that fetch URLs use it too.
type PublicDialer struct {
	Lookup    func(ctx context.Context, host string) ([]netip.Addr, error) // nil = system resolver
	AllowAddr func(netip.Addr) bool                                        // nil = PublicAddr
	Timeout   time.Duration
}

var errPrivate = errors.New("resolves to a local or private address")

func (d PublicDialer) Dial(ctx context.Context, host string, port int) (net.Conn, error) {
	lookup, allow := d.Lookup, d.AllowAddr
	if lookup == nil {
		lookup = func(ctx context.Context, h string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
		}
	}
	if allow == nil {
		allow = PublicAddr
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil, fmt.Errorf("%s: IP addresses aren't allowed", host)
	}
	addrs, err := lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s: no addresses", host)
	}
	for _, a := range addrs {
		if !allow(a) {
			return nil, fmt.Errorf("%s %w (%s)", host, errPrivate, a)
		}
	}
	nd := net.Dialer{Timeout: d.Timeout}
	if nd.Timeout == 0 {
		nd.Timeout = 15 * time.Second
	}
	var last error
	for _, a := range addrs {
		c, err := nd.DialContext(ctx, "tcp", netip.AddrPortFrom(a.Unmap(), uint16(port)).String())
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

// EgressEvent is one proxy decision; the runner puts these in the run's trace.
type EgressEvent struct {
	RunID   string
	Host    string
	Port    int
	Allowed bool
	Reason  string
}

// Egress is the daemon's one HTTP(S) proxy for sandboxed scripts (SECURITY.md "Egress proxy").
// Each run gets its own credential and allowlist; Revoke cuts off anything that outlives the run.
type Egress struct {
	Dialer PublicDialer
	Log    func(EgressEvent)

	ln     net.Listener
	srv    *http.Server
	mu     sync.Mutex
	grants map[string]grant
}

type grant struct {
	runID string
	allow perms.Set
	ports []int // CONNECT ports; default 443. Plain http always uses 80.
}

// StartEgress listens on 127.0.0.1 on a free port.
func StartEgress() (*Egress, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	e := &Egress{ln: ln, grants: map[string]grant{}}
	e.srv = &http.Server{Handler: e, ReadHeaderTimeout: 10 * time.Second}
	go e.srv.Serve(ln)
	return e, nil
}

func (e *Egress) Port() int { return e.ln.Addr().(*net.TCPAddr).Port }

func (e *Egress) Close() error { return e.srv.Close() }

// Register opens network access for one run and returns its credential.
func (e *Egress) Register(runID string, allow perms.Set, ports []int) string {
	b := make([]byte, 16)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	if len(ports) == 0 {
		ports = []int{443}
	}
	e.mu.Lock()
	e.grants[tok] = grant{runID: runID, allow: allow, ports: ports}
	e.mu.Unlock()
	return tok
}

func (e *Egress) Revoke(tok string) {
	e.mu.Lock()
	delete(e.grants, tok)
	e.mu.Unlock()
}

// URL is the proxy URL a script gets in HTTPS_PROXY and HTTP_PROXY.
func (e *Egress) URL(tok string) string {
	return fmt.Sprintf("http://run:%s@127.0.0.1:%d", tok, e.Port())
}

func (e *Egress) lookupGrant(r *http.Request) (grant, bool) {
	h := r.Header.Get("Proxy-Authorization")
	enc, ok := strings.CutPrefix(h, "Basic ")
	if !ok {
		return grant{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return grant{}, false
	}
	_, tok, _ := strings.Cut(string(raw), ":")
	e.mu.Lock()
	defer e.mu.Unlock()
	g, ok := e.grants[tok]
	return g, ok
}

func (e *Egress) log(ev EgressEvent) {
	if e.Log != nil {
		e.Log(ev)
	}
}

func (e *Egress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g, ok := e.lookupGrant(r)
	if !ok {
		w.Header().Set("Proxy-Authenticate", `Basic realm="noskill"`)
		http.Error(w, "noskill egress: no valid run credential", http.StatusProxyAuthRequired)
		return
	}
	var host string
	var port int
	if r.Method == http.MethodConnect {
		h, p, err := net.SplitHostPort(r.Host)
		if err != nil {
			http.Error(w, "bad CONNECT target", http.StatusBadRequest)
			return
		}
		host = h
		port, _ = strconv.Atoi(p)
	} else {
		if r.URL.Scheme != "http" || r.URL.Host == "" {
			http.Error(w, "noskill egress: only absolute http:// requests and CONNECT", http.StatusBadRequest)
			return
		}
		host, port = r.URL.Hostname(), 80
		if p := r.URL.Port(); p != "" {
			port, _ = strconv.Atoi(p)
		}
	}
	refuse := func(code int, why string) {
		e.log(EgressEvent{RunID: g.runID, Host: host, Port: port, Reason: why})
		http.Error(w, "noskill egress: "+why, code)
	}
	if err := g.allow.CheckHost(host); err != nil {
		refuse(http.StatusForbidden, err.Error())
		return
	}
	if (r.Method == http.MethodConnect && !slices.Contains(g.ports, port)) || (r.Method != http.MethodConnect && port != 80) {
		refuse(http.StatusForbidden, fmt.Sprintf("port %d isn't allowed", port))
		return
	}
	conn, err := e.Dialer.Dial(r.Context(), host, port)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, errPrivate) {
			code = http.StatusForbidden
		}
		refuse(code, err.Error())
		return
	}
	e.log(EgressEvent{RunID: g.runID, Host: host, Port: port, Allowed: true})
	if r.Method == http.MethodConnect {
		e.tunnel(w, conn)
		return
	}
	e.forward(w, r, conn)
}

func (e *Egress) tunnel(w http.ResponseWriter, up net.Conn) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	down.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() {
		if buf.Reader.Buffered() > 0 {
			io.CopyN(up, buf, int64(buf.Reader.Buffered()))
		}
		io.Copy(up, down)
		up.Close()
	}()
	io.Copy(down, up)
	down.Close()
}

func (e *Egress) forward(w http.ResponseWriter, r *http.Request, conn net.Conn) {
	used := false
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if used {
				return nil, errors.New("one connection per proxied request")
			}
			used = true
			return conn, nil
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Proxy-Connection")
	resp, err := tr.RoundTrip(out)
	if err != nil {
		http.Error(w, "noskill egress: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
