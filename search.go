package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

// Independently implemented against DDG's HTML interface, using SearXNG's
// duckduckgo.py as a reference for form parameters, selectors and CAPTCHA checks.
const ddgURL = "https://html.duckduckgo.com/html/"
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:157.0) Gecko/20100101 Firefox/157.0"

var resultSelector = cascadia.MustCompile("#links .web-result")
var titleSelector = cascadia.MustCompile("h2 a")
var snippetSelector = cascadia.MustCompile(".result__snippet")
var challengeSelector = cascadia.MustCompile("form#challenge-form")
var noResultsSelector = cascadia.MustCompile(".no-results")

// DDG names regions territory-language, whereas the existing API uses BCP47.
var regions = map[string]string{"en": "us-en", "zh": "cn-zh", "zh-cn": "cn-zh", "zh-tw": "tw-tzh", "zh-hk": "hk-tzh", "en-us": "us-en", "en-gb": "uk-en", "en-au": "au-en", "en-ca": "ca-en", "en-in": "in-en", "en-sg": "sg-en", "fr": "fr-fr", "fr-fr": "fr-fr", "fr-ca": "ca-fr", "de": "de-de", "de-de": "de-de", "es": "es-es", "es-es": "es-es", "es-mx": "mx-es", "it": "it-it", "it-it": "it-it", "ja": "jp-jp", "ja-jp": "jp-jp", "ko": "kr-kr", "ko-kr": "kr-kr", "pt": "pt-pt", "pt-pt": "pt-pt", "pt-br": "br-pt", "ru": "ru-ru", "ru-ru": "ru-ru", "nl": "nl-nl", "nl-nl": "nl-nl", "pl": "pl-pl", "tr": "tr-tr", "sv": "se-sv", "id": "id-id", "vi": "vn-en", "th": "th-th"}

type searchInput struct {
	Query            string
	Max              int
	Language, Period string
}
type result struct {
	Title   string   `json:"title"`
	URL     string   `json:"url"`
	Content string   `json:"content"`
	Snippet string   `json:"snippet"`
	Engines []string `json:"engines"`
}
type searchResponse struct {
	Query        string     `json:"query"`
	Results      []result   `json:"results"`
	ResponseTime float64    `json:"response_time"`
	Backend      string     `json:"backend"`
	Unresponsive [][]string `json:"unresponsive_engines"`
	Suggestions  []string   `json:"suggestions"`
}
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }
func bad(message string) error    { return &apiError{400, message} }

func parseInput(args map[string]any) (searchInput, error) {
	in := searchInput{Max: 6, Language: "auto"}
	q, ok := args["query"]
	if !ok {
		q = args["q"]
	}
	in.Query, ok = q.(string)
	if !ok || strings.TrimSpace(in.Query) == "" {
		return in, bad("query must be a non-empty string")
	}
	in.Query = strings.TrimSpace(in.Query)
	if utf8.RuneCountInString(in.Query) > 499 {
		return in, bad("query must be at most 499 characters")
	}
	if value, exists := args["max_results"]; exists {
		switch v := value.(type) {
		case string:
			n, err := strconv.Atoi(v)
			if err != nil {
				return in, bad("max_results must be an integer")
			}
			in.Max = n
		case jsonNumber:
			n, err := strconv.Atoi(string(v))
			if err != nil {
				return in, bad("max_results must be an integer")
			}
			in.Max = n
		default:
			return in, bad("max_results must be an integer")
		}
	}
	if in.Max < 1 || in.Max > 20 {
		return in, bad("max_results must be between 1 and 20")
	}
	if v, exists := args["language"]; exists {
		var ok bool
		in.Language, ok = v.(string)
		if !ok {
			return in, bad("language must be a string")
		}
		in.Language = strings.ToLower(in.Language)
	}
	if in.Language != "auto" && in.Language != "all" {
		if _, ok := regions[in.Language]; !ok {
			return in, bad("unsupported DuckDuckGo language/region: " + in.Language)
		}
	}
	if v, exists := args["time_range"]; exists && v != nil {
		var ok bool
		in.Period, ok = v.(string)
		if !ok {
			return in, bad("time_range must be a string")
		}
	}
	if in.Period != "" && map[string]string{"day": "d", "week": "w", "month": "m", "year": "y"}[in.Period] == "" {
		return in, bad("time_range must be day, week, month, or year")
	}
	return in, nil
}

func languageFor(in searchInput) string {
	if in.Language != "auto" {
		return in.Language
	}
	for _, r := range in.Query {
		if unicode.Is(unicode.Han, r) {
			return "zh-CN"
		}
	}
	return "en"
}

func formFor(in searchInput) url.Values {
	region := "wt-wt"
	if r, ok := regions[strings.ToLower(languageFor(in))]; ok {
		region = r
	}
	// Quote leading bangs to keep searches within the search endpoint.
	words := strings.Fields(in.Query)
	for i, w := range words {
		if strings.HasPrefix(w, "!") {
			words[i] = "'" + w + "'"
		}
	}
	v := url.Values{"q": {strings.Join(words, " ")}, "b": {""}, "kl": {region}}
	if in.Period != "" {
		v.Set("df", map[string]string{"day": "d", "week": "w", "month": "m", "year": "y"}[in.Period])
	}
	return v
}
func text(n *html.Node) string {
	if n == nil {
		return ""
	}
	var out strings.Builder
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node.Type == html.TextNode {
			out.WriteString(node.Data)
		}
		if node.Type == html.ElementNode && (node.Data == "br" || node.Data == "p") {
			out.WriteByte(' ')
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(n)
	return strings.Join(strings.Fields(out.String()), " ")
}
func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}
func canonicalURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme == "" && u.Host != "" {
		u.Scheme = "https"
	}
	if (u.Hostname() == "duckduckgo.com" || u.Hostname() == "www.duckduckgo.com") && u.Path == "/l/" {
		target := u.Query().Get("uddg")
		if target == "" {
			return ""
		}
		u, err = url.Parse(target)
		if err != nil {
			return ""
		}
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	q := u.Query()
	for k := range q {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "utm_") || lower == "msclkid" || lower == "fbclid" {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String()
}
func parseResults(body io.Reader, maximum int) ([]result, error) {
	doc, err := html.Parse(body)
	if err != nil {
		return nil, err
	}
	if cascadia.Query(doc, challengeSelector) != nil {
		return nil, fmt.Errorf("DuckDuckGo CAPTCHA challenge")
	}
	results := []result{}
	seen := map[string]bool{}
	for _, block := range cascadia.QueryAll(doc, resultSelector) {
		// Ads must never be merged with organic web results.
		classes := strings.Fields(attr(block, "class"))
		ad := false
		for _, c := range classes {
			if strings.HasPrefix(c, "result--ad") {
				ad = true
			}
		}
		if ad {
			continue
		}
		link := cascadia.Query(block, titleSelector)
		if link == nil {
			continue
		}
		title := text(link)
		u := canonicalURL(attr(link, "href"))
		if title == "" || u == "" || seen[u] {
			continue
		}
		seen[u] = true
		snippet := text(cascadia.Query(block, snippetSelector))
		results = append(results, result{Title: title, URL: u, Content: snippet, Snippet: snippet, Engines: []string{"duckduckgo"}})
		if len(results) >= maximum {
			break
		}
	}
	if len(results) == 0 && cascadia.Query(doc, noResultsSelector) == nil {
		return nil, fmt.Errorf("DuckDuckGo returned no recognizable results (unexpected HTML)")
	}
	return results, nil
}
func (a *app) search(ctx context.Context, in searchInput) (searchResponse, error) {
	start := time.Now()
	v := formFor(in)
	form := "q=" + url.QueryEscape(v.Get("q")) + "&b=&kl=" + url.QueryEscape(v.Get("kl"))
	if v.Get("df") != "" {
		form += "&df=" + v.Get("df")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, strings.NewReader(form))
	if err != nil {
		return searchResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", ddgURL)
	for k, val := range map[string]string{"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "same-origin", "Sec-Fetch-User": "?1"} {
		req.Header.Set(k, val)
	}
	if lang := languageFor(in); lang != "all" {
		req.Header.Set("Accept-Language", lang+",en;q=0.5")
	}
	if v.Get("kl") != "wt-wt" {
		req.AddCookie(&http.Cookie{Name: "kl", Value: v.Get("kl")})
	}
	if v.Get("df") != "" {
		req.AddCookie(&http.Cookie{Name: "df", Value: v.Get("df")})
	}
	response, err := a.client.Do(req)
	if err != nil {
		return searchResponse{}, &apiError{502, "DuckDuckGo request failed: " + err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return searchResponse{}, &apiError{502, fmt.Sprintf("DuckDuckGo returned HTTP %d", response.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil {
		return searchResponse{}, &apiError{502, "DuckDuckGo response read failed: " + err.Error()}
	}
	if len(data) > 2*1024*1024 {
		return searchResponse{}, &apiError{502, "DuckDuckGo response exceeded 2 MiB"}
	}
	results, err := parseResults(strings.NewReader(string(data)), in.Max)
	if err != nil {
		return searchResponse{}, &apiError{502, err.Error()}
	}
	return searchResponse{Query: in.Query, Results: results, ResponseTime: time.Since(start).Seconds(), Backend: "duckduckgo-go", Unresponsive: [][]string{}, Suggestions: []string{}}, nil
}
