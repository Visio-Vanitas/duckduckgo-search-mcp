package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixture = `<div id="links"><div class="result web-result result--ad"><h2><a href="https://ads.example">Ad</a></h2></div><div class="result web-result"><h2><a href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fdocs.example%2Fpage%3Fid%3D42%26utm_source%3Dddg">Official <b>docs</b> &amp; guide</a></h2><a class="result__snippet">Use <b>TaskGroup</b> safely.</a></div><div class="web-result"><h2><a href="https://docs.example/page?id=42#duplicate">Duplicate</a></h2></div><div class="web-result"><h2><a href="javascript:bad">Invalid URL</a></h2></div><div class="web-result"><h2><a href="https://other.example">Other</a></h2><a class="result__snippet">Second.</a></div></div>`

func TestOrganicParsing(t *testing.T) {
	r, err := parseResults(strings.NewReader(fixture), 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != 2 || r[0].URL != "https://docs.example/page?id=42" || r[0].Title != "Official docs & guide" || r[0].Content != "Use TaskGroup safely." {
		t.Fatalf("wrong results %+v", r)
	}
}
func TestCaptchaAndUnexpectedHTML(t *testing.T) {
	for _, body := range []string{`<form id="challenge-form"></form>`, `<h1>Service unavailable</h1>`} {
		if _, err := parseResults(strings.NewReader(body), 6); err == nil {
			t.Fatal("upstream failure became successful empty results")
		}
	}
	r, err := parseResults(strings.NewReader(`<div class="no-results">No results</div>`), 6)
	if err != nil || len(r) != 0 {
		t.Fatal(r, err)
	}
}
func TestInputAndForm(t *testing.T) {
	in, err := parseInput(map[string]any{"query": "Python asyncio TaskGroup 文档", "max_results": json.Number("3"), "language": "zh-CN", "time_range": "week"})
	if err != nil {
		t.Fatal(err)
	}
	v := formFor(in)
	if v.Get("q") != in.Query || v.Get("kl") != "cn-zh" || v.Get("df") != "w" {
		t.Fatal(v)
	}
	for _, n := range []any{true, 1.5, json.Number("1.5"), json.Number("0")} {
		if _, err := parseInput(map[string]any{"query": "q", "max_results": n}); err == nil {
			t.Fatalf("accepted %v", n)
		}
	}
}
func TestReadOnlyAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE tokens (key TEXT,status INTEGER,remain_quota INTEGER,unlimited_quota INTEGER,expired_time INTEGER,deleted_at INTEGER); INSERT INTO tokens VALUES ('unlimited',1,0,1,-1,NULL),('expired',1,100,0,1,NULL),('disabled',2,100,0,-1,NULL),('deleted',1,100,0,-1,1),('',1,0,1,-1,NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	ro, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	a := app{db: ro}
	for token, want := range map[string]bool{"": false, "unlimited": true, "expired": false, "disabled": false, "deleted": false, "missing": false} {
		ok, err := a.authorized(context.Background(), "Bearer sk-"+token)
		if err != nil || ok != want {
			t.Fatalf("%s: %v %v", token, ok, err)
		}
	}
	if _, err := ro.Exec(`INSERT INTO tokens (key) VALUES ('write')`); err == nil {
		t.Fatal("auth connection is writable")
	}
}
func TestRESTAndMCP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Error("wrong upstream method")
		}
		raw, _ := io.ReadAll(r.Body)
		if !strings.HasPrefix(string(raw), "q=") {
			t.Error("DDG form must start with query")
		}
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		r.ParseForm()
		if r.PostForm.Get("q") == "fail" {
			w.WriteHeader(429)
			return
		}
		w.Write([]byte(fixture))
	}))
	defer upstream.Close()
	a := app{searchSlots: make(chan struct{}, 4), apiKey: "test-secret", client: &http.Client{Timeout: time.Second}, endpoint: upstream.URL}
	tests := []struct {
		path, body string
		status     int
		check      string
	}{
		{"/v1/search", `{"query":"docs","max_results":1}`, 200, `"backend":"duckduckgo-go"`},
		{"/v1/search", `{"query":"fail"}`, 502, "HTTP 429"},
		{"/v1/search", `{"query":""}`, 400, "non-empty"},
		{"/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, 200, "protocolVersion"},
		{"/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 200, "web_search"},
		{"/mcp/search", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"fail"}}}`, 200, `"isError":true`},
		{"/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, 204, ""},
		{"/mcp", `{"jsonrpc":"2.0","id":3,"method":"ping"}`, 200, `"result":{}`},
		{"/mcp", `{"jsonrpc":"2.0","method":"unknown","id":3}`, 200, "-32601"},
	}
	for _, test := range tests {
		r := httptest.NewRequest("POST", test.path, strings.NewReader(test.body))
		r.Header.Set("Authorization", "Bearer test-secret")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != test.status || !strings.Contains(w.Body.String(), test.check) {
			t.Fatalf("%s: %d %s", test.body, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "/v1/search?q=docs&max_results=1", nil)
	r.Header.Set("Authorization", "Bearer test-secret")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

func TestBrowserTransportBodyAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 3 {
			t.Errorf("lost content length: %d", r.ContentLength)
		}
		w.Write([]byte("ok"))
	}))
	defer server.Close()
	transport, err := newBrowserTransport("")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport, Timeout: time.Second}
	req, _ := http.NewRequest("POST", server.URL, strings.NewReader("q=x"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ = http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	if _, err = client.Do(req); err == nil {
		t.Fatal("canceled request succeeded")
	}
}

func TestAutomaticLanguageRegion(t *testing.T) {
	for query, expected := range map[string]string{"Python asyncio TaskGroup 文档": "cn-zh", "Go runtime docs": "us-en"} {
		v := formFor(searchInput{Query: query, Language: "auto"})
		if v.Get("kl") != expected {
			t.Fatalf("%s region %s", query, v.Get("kl"))
		}
	}
	if formFor(searchInput{Query: "中文", Language: "all"}).Get("kl") != "wt-wt" {
		t.Fatal("explicit all region changed")
	}
}

func TestConfiguredAPIKey(t *testing.T) {
	a := app{apiKey: "test-secret"}
	for header, want := range map[string]bool{"": false, "Bearer wrong": false, "Bearer test-secret": true, "Basic test-secret": false, "Bearer sk-test-secret": false} {
		ok, err := a.authorized(context.Background(), header)
		if err != nil || ok != want {
			t.Fatalf("header %q: %v %v", header, ok, err)
		}
	}
	for _, tc := range []struct {
		key, db string
		valid   bool
	}{{"", "", false}, {"key", "", true}, {"", "tokens.db", true}, {"key", "tokens.db", false}} {
		if (validateAuthConfig(tc.key, tc.db) == nil) != tc.valid {
			t.Fatalf("unexpected auth configuration result: %+v", tc)
		}
	}
}

func TestAPIKeyModeHealthAndRejectsMissingAuth(t *testing.T) {
	a := app{apiKey: "test-secret"}
	for _, path := range []string{"/v1/search?q=docs", "/mcp"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
