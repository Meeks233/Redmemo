package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/render"
)

func TestRobotsTxt_IndexingOff(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	req := httptest.NewRequest("GET", "/robots.txt", nil)
	w := httptest.NewRecorder()
	h.handleRobotsTxt(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "Disallow: /") {
		t.Fatalf("expected blanket Disallow when indexing off, got: %q", body)
	}
	if strings.Contains(body, "Sitemap:") {
		t.Errorf("did not expect Sitemap reference when indexing off, got: %q", body)
	}
}

func TestRobotsTxt_IndexingOn(t *testing.T) {
	cfg := &config.Config{SEO: config.SEOConfig{AllowIndexing: true, CanonicalHost: "https://memo.example.com"}}
	h := &Handler{cfg: cfg}
	req := httptest.NewRequest("GET", "/robots.txt", nil)
	w := httptest.NewRecorder()
	h.handleRobotsTxt(w, req)

	body := w.Body.String()
	wantStrings := []string{
		"Allow: /archive",
		"Disallow: /r/",
		"Disallow: /user/",
		"Disallow: /settings",
		"Disallow: /api/",
		"Sitemap: https://memo.example.com/sitemap.xml",
	}
	for _, want := range wantStrings {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in robots.txt:\n%s", want, body)
		}
	}
}

func TestSitemapXML_404WhenIndexingOff(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	req := httptest.NewRequest("GET", "/sitemap.xml", nil)
	w := httptest.NewRecorder()
	h.handleSitemapXML(w, req)
	if w.Code != 404 {
		t.Errorf("expected 404 when indexing off, got %d", w.Code)
	}
}

func TestArchiveHubDescription(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}

	if got := h.archiveHubDescription("RedMemo", nil); !strings.Contains(got, "RedMemo") {
		t.Errorf("empty-subs description should still mention brand, got: %q", got)
	}

	got := h.archiveHubDescription("RedMemo", []string{"golang", "rust", "linux"})
	for _, want := range []string{"r/golang", "r/rust", "r/linux", "RedMemo"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in description: %q", want, got)
		}
	}

	subs := make([]string, archiveDescSubCap+5)
	for i := range subs {
		subs[i] = "sub" + string(rune('A'+i))
	}
	got = h.archiveHubDescription("RedMemo", subs)
	if !strings.Contains(got, "and 5 more") {
		t.Errorf("overflow should mention remainder, got: %q", got)
	}
}

func TestDecorateArchiveHubSEO_NoopWhenOff(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	d := &render.ArchiveHubPageData{}
	h.decorateArchiveHubSEO(d, []string{"golang"})
	if d.Indexable || d.MetaDescription != "" || d.HeadExtraHTML != "" {
		t.Errorf("expected no SEO stamping when indexing off, got %+v", d)
	}
}

func TestDecorateArchiveHubSEO_SkipsSearchPages(t *testing.T) {
	h := &Handler{cfg: &config.Config{SEO: config.SEOConfig{AllowIndexing: true}}}
	d := &render.ArchiveHubPageData{Search: true}
	h.decorateArchiveHubSEO(d, []string{"golang"})
	if d.Indexable {
		t.Error("search result pages should not be marked Indexable")
	}
}

func TestDecorateArchiveHubSEO_StampsHubPage(t *testing.T) {
	h := &Handler{cfg: &config.Config{SEO: config.SEOConfig{AllowIndexing: true, CanonicalHost: "https://memo.example.com"}}}
	d := &render.ArchiveHubPageData{BasePage: render.BasePage{BrandName: "RedMemo"}}
	h.decorateArchiveHubSEO(d, []string{"golang", "rust"})

	if !d.Indexable {
		t.Error("expected Indexable=true")
	}
	if !strings.Contains(d.MetaDescription, "r/golang") {
		t.Errorf("description missing sub: %q", d.MetaDescription)
	}
	if !strings.Contains(d.HeadExtraHTML, "application/ld+json") {
		t.Errorf("HeadExtraHTML missing JSON-LD: %q", d.HeadExtraHTML)
	}
	if !strings.Contains(d.HeadExtraHTML, `rel="canonical"`) {
		t.Errorf("HeadExtraHTML missing canonical: %q", d.HeadExtraHTML)
	}
	if !strings.Contains(d.HeadExtraHTML, "memo.example.com/archive/r/golang") {
		t.Errorf("JSON-LD missing absolute sub URL: %q", d.HeadExtraHTML)
	}
}

func TestDecorateArchiveSubSEO(t *testing.T) {
	h := &Handler{cfg: &config.Config{SEO: config.SEOConfig{AllowIndexing: true, CanonicalHost: "https://memo.example.com"}}}
	d := &render.ArchivePageData{
		BasePage:   render.BasePage{BrandName: "RedMemo"},
		Sub:        "golang",
		TotalPosts: 4242,
	}
	h.decorateArchiveSubSEO(d, 0, 25)
	if !d.Indexable {
		t.Error("expected Indexable=true")
	}
	if !strings.Contains(d.MetaDescription, "r/golang") || !strings.Contains(d.MetaDescription, "4242") {
		t.Errorf("description missing fields: %q", d.MetaDescription)
	}
	if !strings.Contains(d.HeadExtraHTML, "memo.example.com/archive/r/golang") {
		t.Errorf("canonical/og missing absolute URL: %q", d.HeadExtraHTML)
	}
}
