package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestURLQueryBytesPreserved(t *testing.T) {
	raw := "https://example.com/view?sig=a%20b&part=1;2&repeat=z&repeat=a&utm_source=search"
	want := "https://example.com/view?sig=a%20b&part=1;2&repeat=z&repeat=a"
	if got := canonicalURL(raw); got != want {
		t.Fatalf("query changed: %s", got)
	}
}

func TestBearerSchemeAndWhitespaceKey(t *testing.T) {
	a := app{apiKey: "secret"}
	if ok, err := a.authorized(context.Background(), "bearer secret"); err != nil || !ok {
		t.Fatal("Bearer scheme should be case insensitive", err)
	}
	if err := validateAuthConfig(" secret ", ""); err == nil {
		t.Fatal("accepted a key that cannot match trimmed Authorization tokens")
	}
}

func TestRPCEnvelopeValidation(t *testing.T) {
	a := app{apiKey: "secret"}
	for _, body := range []string{`{"method":"ping","id":1}`, `{"jsonrpc":"2.0","method":"ping","id":{}}`} {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		var response struct {
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		json.Unmarshal(w.Body.Bytes(), &response)
		if response.Error == nil || response.Error.Code != -32600 {
			t.Fatalf("accepted invalid envelope: %s", w.Body.String())
		}
	}
}

func TestSocksHandshakeCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	transport, err := newBrowserTransport("socks5h://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/", nil)
	done := make(chan error, 1)
	go func() {
		resp, err := (&http.Client{Transport: transport}).Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy was not contacted")
	}
	defer conn.Close()
	// Let the SOCKS greeting start, then cancel while the proxy never answers.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var greeting [3]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("canceled handshake succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("SOCKS handshake ignored cancellation")
	}
}

func TestProxyNormalization(t *testing.T) {
	for raw, want := range map[string]string{"http://localhost": "localhost:80", "https://[::1]": "[::1]:443", "socks5h://localhost:1080": "localhost:1080"} {
		normalized, address, err := normalizeProxy(raw)
		if err != nil || address != want {
			t.Fatalf("%s: %s %v", raw, address, err)
		}
		if normalized == "" {
			t.Fatal("lost proxy URL")
		}
	}
	for _, raw := range []string{"socks5://localhost", "http://localhost:0", "http://localhost:70000", "http://localhost/path", "ftp://localhost"} {
		if _, _, err := normalizeProxy(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestRPCFailuresAndNotifications(t *testing.T) {
	a := app{apiKey: "secret"}
	cases := []struct {
		body string
		code int
	}{
		{`{`, -32700}, {`[]`, -32600}, {`{"jsonrpc":[],"method":"ping","id":1}`, -32600},
		{`{"jsonrpc":"2.0","method":"ping","id":false}`, -32600},
		{`{"jsonrpc":"2.0","method":"tools/call","params":[],"id":9}`, -32602},
		{`{"jsonrpc":"2.0","method":"ping","params":false,"id":9}`, -32602},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		var got struct {
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Error.Code != tc.code {
			t.Fatalf("%s: %s", tc.body, w.Body.String())
		}
	}
	// A tool notification must not perform a search (there is deliberately no client).
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"web_search","arguments":{"query":"docs"}}}`))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 204 || w.Body.Len() != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "/mcp?method=ping", nil)
	r.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 405 || w.Header().Get("Allow") != "POST, OPTIONS" {
		t.Fatal(w.Code, w.Header())
	}
	r = httptest.NewRequest("POST", "/mcp", strings.NewReader(strings.Repeat("x", 65537)))
	r.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatal(w.Code)
	}
}

func TestSearchConcurrencyAndCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.PostForm.Get("q") == "hold" {
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.Write([]byte(fixture))
	}))
	defer upstream.Close()
	a := app{client: upstream.Client(), endpoint: upstream.URL, searchSlots: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.search(ctx, searchInput{Query: "hold", Max: 1, Language: "auto"}); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request never started")
	}
	limited, cancelLimited := context.WithTimeout(context.Background(), time.Second)
	defer cancelLimited()
	_, err := a.search(limited, searchInput{Query: "docs", Max: 1, Language: "auto"})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.status != 503 {
		t.Fatalf("saturated search did not fail fast: %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled search did not release slot")
	}
	if _, err := a.search(limited, searchInput{Query: "docs", Max: 1, Language: "auto"}); err != nil {
		t.Fatalf("slot not released: %v", err)
	}
}

func TestDecodedResponseLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		z := gzip.NewWriter(w)
		defer z.Close()
		if r.URL.Path == "/large" {
			io.WriteString(z, strings.Repeat("x", 2*1024*1024+1))
		} else {
			io.WriteString(z, fixture)
		}
	}))
	defer upstream.Close()
	transport, err := newBrowserTransport("")
	if err != nil {
		t.Fatal(err)
	}
	a := app{client: &http.Client{Transport: transport, Timeout: 3 * time.Second}, endpoint: upstream.URL, searchSlots: make(chan struct{}, 1)}
	in := searchInput{Query: "docs", Max: 1, Language: "auto"}
	if _, err := a.search(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	a.endpoint = upstream.URL + "/large"
	if _, err := a.search(context.Background(), in); err == nil || !strings.Contains(err.Error(), "2 MiB") {
		t.Fatalf("decoded size limit failed: %v", err)
	}
}
