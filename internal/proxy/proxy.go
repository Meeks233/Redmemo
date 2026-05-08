package proxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/redmemo/redmemo/internal/config"
)

var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

type Proxy struct {
	upstream   *url.URL
	httpClient *http.Client
}

func New(cfg config.RedlibConfig) (*Proxy, error) {
	u, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("proxy: parse upstream %q: %w", cfg.Upstream, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("proxy: upstream %q must have scheme and host", cfg.Upstream)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &Proxy{
		upstream:   u,
		httpClient: client,
	}, nil
}

// Forward sends the request to the upstream redlib instance and returns
// the full response with body read into memory. The caller is responsible
// for checking rate-limit status via IsRateLimited.
func (p *Proxy) Forward(r *http.Request) (*http.Response, []byte, error) {
	outReq := r.Clone(r.Context())
	outReq.URL.Scheme = p.upstream.Scheme
	outReq.URL.Host = p.upstream.Host
	outReq.Host = p.upstream.Host
	outReq.RequestURI = ""

	removeHopByHop(outReq.Header)

	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		outReq.Header.Set("X-Forwarded-For", clientIP)
	} else {
		outReq.Header.Set("X-Forwarded-For", r.RemoteAddr)
	}

	resp, err := p.httpClient.Do(outReq)
	if err != nil {
		return nil, nil, fmt.Errorf("proxy: forward: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, fmt.Errorf("proxy: read body: %w", err)
	}

	return resp, body, nil
}

func removeHopByHop(h http.Header) {
	for _, k := range hopByHopHeaders {
		h.Del(k)
	}
}
