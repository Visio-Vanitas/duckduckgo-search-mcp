package main

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// Match browser TLS/HTTP2 behavior, as SearXNG does through curl_cffi.
// No Python, browser process, external helper or runtime container is needed.
type browserTransport struct{ client tlsclient.HttpClient }

func newBrowserTransport(proxyURL string) (http.RoundTripper, error) {
	timeout := 30 * time.Second
	options := []tlsclient.HttpClientOption{
		tlsclient.WithClientProfile(profiles.Chrome_150), tlsclient.WithTimeoutSeconds(12), tlsclient.WithDisableHttp3(),
		tlsclient.WithNotFollowRedirects(),
		tlsclient.WithTransportOptions(&tlsclient.TransportOptions{DisableKeepAlives: true, MaxIdleConns: 4, MaxIdleConnsPerHost: 4, MaxConnsPerHost: 4, IdleConnTimeout: &timeout}),
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "socks5" || u.Scheme == "socks5h" {
		// The dependency's default SOCKS wrapper calls Dial without a context.
		options = append(options, tlsclient.WithProxyDialerFactory(func(_ string, timeout time.Duration, local *net.TCPAddr, _ fhttp.Header, _ tlsclient.Logger) (proxy.ContextDialer, error) {
			var auth *proxy.Auth
			if u.User != nil {
				password, _ := u.User.Password()
				auth = &proxy.Auth{User: u.User.Username(), Password: password}
			}
			dialer, err := proxy.SOCKS5("tcp", u.Host, auth, &net.Dialer{Timeout: timeout, LocalAddr: local})
			if err != nil {
				return nil, err
			}
			return dialer.(proxy.ContextDialer), nil
		}))
	} else {
		options = append(options, tlsclient.WithProxyUrl(proxyURL))
	}
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, err
	}
	return &browserTransport{client: client}, nil
}
func (t *browserTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r, err := fhttp.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), req.Body)
	if err != nil {
		return nil, err
	}
	r.ContentLength = req.ContentLength
	for k, v := range req.Header {
		r.Header[strings.ToLower(k)] = v
	}
	r.Header["sec-ch-ua"] = []string{`"Not;A=Brand";v="8", "Chromium";v="150", "Google Chrome";v="150"`}
	r.Header["sec-ch-ua-mobile"] = []string{"?0"}
	r.Header["sec-ch-ua-platform"] = []string{`"macOS"`}
	r.Header["upgrade-insecure-requests"] = []string{"1"}
	r.Header["accept"] = []string{"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"}
	r.Header["accept-encoding"] = []string{"gzip, deflate, br, zstd"}
	r.Header["priority"] = []string{"u=0, i"}
	r.Header[fhttp.HeaderOrderKey] = []string{"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "upgrade-insecure-requests", "user-agent", "accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest", "accept-encoding", "accept-language", "priority", "referer", "content-type", "content-length", "cookie"}

	response, err := t.client.Do(r)
	if err != nil {
		return nil, err
	}
	if response.Header.Get("Content-Encoding") != "" && !response.Uncompressed {
		response.Body = fhttp.DecompressBody(response)
		response.Header.Del("Content-Encoding")
	}
	return &http.Response{StatusCode: response.StatusCode, Status: response.Status, Header: http.Header(response.Header), Body: response.Body, ContentLength: response.ContentLength, Request: req}, nil
}
