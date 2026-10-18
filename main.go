package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type jsonNumber = json.Number
type app struct {
	apiKey                 string
	db                     *sql.DB
	client                 *http.Client
	endpoint, proxyAddress string
}

func (a *app) authorized(ctx context.Context, header string) (bool, error) {
	if !strings.HasPrefix(header, "Bearer ") {
		return false, nil
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if a.apiKey != "" {
		return subtle.ConstantTimeCompare([]byte(token), []byte(a.apiKey)) == 1, nil
	}
	key := strings.TrimPrefix(token, "sk-")
	var status, remaining, unlimited, expiry int64
	err := a.db.QueryRowContext(ctx, "SELECT status,remain_quota,unlimited_quota,expired_time FROM tokens WHERE key=? AND deleted_at IS NULL LIMIT 1", key).Scan(&status, &remaining, &unlimited, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return status == 1 && (remaining > 0 || unlimited != 0) && (expiry == -1 || expiry > time.Now().Unix()), nil
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			log.Printf("response write failed: %v", err)
		}
	}
}
func replyError(w http.ResponseWriter, err error) {
	var e *apiError
	if errors.As(err, &e) {
		reply(w, e.status, map[string]any{"error": e.message})
	} else {
		log.Printf("dependency failure: %v", err)
		reply(w, 503, map[string]any{"error": "Search dependency unavailable"})
	}
}
func readArgs(r *http.Request) (map[string]any, error) {
	if r.Method == http.MethodGet {
		args := map[string]any{}
		for k, v := range r.URL.Query() {
			if len(v) > 0 {
				args[k] = v[0]
			}
		}
		return args, nil
	}
	if r.Method != http.MethodPost {
		return nil, &apiError{405, "Method not allowed"}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 65537))
	if err != nil {
		return nil, bad("Invalid request body")
	}
	if len(raw) == 0 || len(raw) > 65536 {
		return nil, bad("Request body must be 1 to 65536 bytes")
	}
	var args map[string]any
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if err := d.Decode(&args); err != nil || args == nil {
		return nil, bad("JSON body must be an object")
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return nil, bad("Invalid trailing JSON")
	}
	return args, nil
}

var schema = map[string]any{"type": "object", "properties": map[string]any{
	"query":       map[string]any{"type": "string", "description": "Search keywords or question"},
	"max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "default": 6},
	"time_range":  map[string]any{"type": "string", "enum": []string{"day", "week", "month", "year"}},
	"language":    map[string]any{"type": "string", "description": "Language/region code, e.g. en or zh-CN"}}, "required": []string{"query"}}

func rpc(w http.ResponseWriter, id any, result any) {
	reply(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func rpcError(w http.ResponseWriter, id any, code int, msg string) {
	reply(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}
func toolError(w http.ResponseWriter, id any, err error) {
	rpc(w, id, map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true})
}
func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	if r.Method == http.MethodOptions {
		reply(w, 204, nil)
		return
	}
	if r.URL.Path == "/health" {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if a.db != nil {
			if err := a.db.PingContext(ctx); err != nil {
				replyError(w, err)
				return
			}
		}
		if a.proxyAddress != "" {
			conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", a.proxyAddress)
			if err != nil {
				replyError(w, err)
				return
			}
			conn.Close()
		}
		reply(w, 200, map[string]any{"status": "healthy", "service": "duckduckgo-search-mcp", "backend": "duckduckgo-go"})
		return
	}
	switch r.URL.Path {
	case "/v1/search", "/mcp", "/mcp/search":
	default:
		reply(w, 404, map[string]any{"error": "Not found"})
		return
	}
	ok, err := a.authorized(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		replyError(w, err)
		return
	}
	if !ok {
		reply(w, 401, map[string]any{"error": "Invalid or unavailable search token"})
		return
	}
	args, err := readArgs(r)
	if err != nil {
		replyError(w, err)
		return
	}
	if r.URL.Path == "/v1/search" {
		in, err := parseInput(args)
		if err != nil {
			replyError(w, err)
			return
		}
		data, err := a.search(r.Context(), in)
		if err != nil {
			replyError(w, err)
			return
		}
		reply(w, 200, data)
		return
	}
	method, ok := args["method"].(string)
	if !ok {
		replyError(w, bad("MCP method must be a string"))
		return
	}
	id := args["id"]
	if strings.HasPrefix(method, "notifications/") {
		reply(w, 204, nil)
		return
	}
	switch method {
	case "initialize":
		rpc(w, id, map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]any{"name": "duckduckgo-search-mcp", "version": "0.1.0"}, "capabilities": map[string]any{"tools": map[string]any{}}})
	case "tools/list":
		rpc(w, id, map[string]any{"tools": []map[string]any{{"name": "web_search", "description": "Search the live web using DuckDuckGo; returns source URLs and snippets.", "inputSchema": schema}}})
	case "ping":
		rpc(w, id, map[string]any{})
	case "tools/call":
		params, ok := args["params"].(map[string]any)
		if !ok {
			replyError(w, bad("MCP params must be an object"))
			return
		}
		if params["name"] != "web_search" {
			rpcError(w, id, -32602, "Unknown tool")
			return
		}
		inputs, ok := params["arguments"].(map[string]any)
		if !ok {
			toolError(w, id, bad("Search arguments must be an object"))
			return
		}
		in, err := parseInput(inputs)
		if err != nil {
			toolError(w, id, err)
			return
		}
		data, err := a.search(r.Context(), in)
		if err != nil {
			toolError(w, id, err)
			return
		}
		raw, err := json.Marshal(data)
		if err != nil {
			replyError(w, err)
			return
		}
		rpc(w, id, map[string]any{"content": []map[string]any{{"type": "text", "text": string(raw)}}, "isError": false})
	default:
		rpcError(w, id, -32601, "Method not found")
	}
}
func validateAuthConfig(apiKey, dbPath string) error {
	if (apiKey == "") == (dbPath == "") {
		return errors.New("configure exactly one of SEARCH_API_KEY or --db")
	}
	return nil
}

func main() {
	port := flag.Int("port", 18085, "Loopback HTTP port")
	dbPath := flag.String("db", "", "Optional read-only new-api token database (alternative to SEARCH_API_KEY)")
	proxyURL := flag.String("proxy", "", "Optional HTTP(S) or SOCKS5(H) upstream proxy")
	flag.Parse()
	apiKey := os.Getenv("SEARCH_API_KEY")
	if err := validateAuthConfig(apiKey, *dbPath); err != nil {
		log.Fatal(err)
	}
	proxyAddress := ""
	if *proxyURL != "" {
		u, err := url.Parse(*proxyURL)
		if err != nil || u.Host == "" {
			log.Fatal("proxy must include a scheme and host")
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			log.Fatal("unsupported proxy scheme")
		}
		proxyAddress = u.Host
	}
	transport, err := newBrowserTransport(*proxyURL)
	if err != nil {
		log.Fatal(err)
	}
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	var db *sql.DB
	if *dbPath != "" {
		dsn := url.URL{Scheme: "file", Path: *dbPath}
		dsn.RawQuery = url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(3000)"}}.Encode()
		db, err = sql.Open("sqlite", dsn.String())
		if err != nil {
			log.Fatal(err)
		}
		defer db.Close()
		db.SetMaxOpenConns(2)
		if err := db.Ping(); err != nil {
			log.Fatal(err)
		}
	}
	a := &app{apiKey: apiKey, db: db, client: client, endpoint: ddgURL, proxyAddress: proxyAddress}

	server := &http.Server{Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)), Handler: a, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	log.Printf("DuckDuckGo search REST/MCP listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}
