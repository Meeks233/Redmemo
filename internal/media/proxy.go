package media

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/redmemo/redmemo/internal/cache"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/store"
)

type Proxy struct {
	rootPath   string
	useNginx   bool
	mediaStore *store.MediaIndexStore
	cache      *cache.Cache
	httpClient *http.Client
}

func NewProxy(cfg config.MediaConfig, mediaStore *store.MediaIndexStore, c *cache.Cache) *Proxy {
	return &Proxy{
		rootPath:   cfg.RootPath,
		mediaStore: mediaStore,
		cache:      c,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *Proxy) ServeMedia(w http.ResponseWriter, r *http.Request) {
	originalURL := r.URL.Query().Get("url")
	if originalURL == "" {
		http.Error(w, "missing url parameter", http.StatusBadRequest)
		return
	}

	meta, err := p.mediaStore.Resolve(originalURL)
	if err != nil {
		log.Printf("media: resolve error for %s: %v", originalURL, err)
		p.reverseProxy(w, r, originalURL)
		return
	}

	if meta != nil && meta.FilePath != nil {
		if _, err := os.Stat(*meta.FilePath); err == nil {
			p.cache.RecordMediaAccess(r.Context(), originalURL)
			p.serve(w, r, meta)
			return
		}
	}

	meta, err = p.Download(r.Context(), originalURL)
	if err != nil {
		log.Printf("media: download failed for %s: %v", originalURL, err)
		p.reverseProxy(w, r, originalURL)
		return
	}

	p.serve(w, r, meta)
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, meta *store.MediaMeta) {
	w.Header().Set("Content-Type", meta.MIMEType)
	w.Header().Set("Cache-Control", "public, max-age=86400")

	if p.useNginx {
		w.Header().Set("X-Accel-Redirect", NginxPath(meta.Hash))
		return
	}

	if meta.FilePath != nil {
		http.ServeFile(w, r, *meta.FilePath)
	}
}

func (p *Proxy) Download(ctx context.Context, originalURL string) (*store.MediaMeta, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", originalURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	hash := HashURL(originalURL)
	filePath := HashToPath(p.rootPath, hash)

	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	f, err := os.Create(filePath)
	if err != nil {
		return nil, fmt.Errorf("create file: %w", err)
	}

	size, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(filePath)
		return nil, fmt.Errorf("write file: %w", err)
	}

	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	meta := &store.MediaMeta{
		OriginalURL: originalURL,
		Hash:        hash,
		FilePath:    &filePath,
		MIMEType:    mimeType,
		FileSize:    size,
	}
	if err := p.mediaStore.Save(meta); err != nil {
		return nil, fmt.Errorf("save index: %w", err)
	}

	return meta, nil
}

func (p *Proxy) DownloadAsync(originalURL string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := p.Download(ctx, originalURL); err != nil {
			log.Printf("media: async download failed for %s: %v", originalURL, err)
		}
	}()
}

func (p *Proxy) reverseProxy(w http.ResponseWriter, r *http.Request, targetURL string) {
	req, err := http.NewRequestWithContext(r.Context(), "GET", targetURL, nil)
	if err != nil {
		http.Error(w, "bad upstream url", http.StatusBadGateway)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	for _, h := range []string{"Range", "If-Modified-Since", "Cache-Control"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
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
