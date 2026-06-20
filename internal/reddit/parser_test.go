package reddit

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseSubredditListing_Empty(t *testing.T) {
	data := `{"kind":"Listing","data":{"before":null,"after":null,"children":[]}}`
	posts, before, after, err := ParseSubredditListing([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(posts) != 0 {
		t.Errorf("expected 0 posts, got %d", len(posts))
	}
	if before != "" || after != "" {
		t.Errorf("expected empty cursors, got before=%q after=%q", before, after)
	}
}

func TestParseSubredditListing_WithPosts(t *testing.T) {
	data := `{
		"kind": "Listing",
		"data": {
			"before": "t3_before",
			"after": "t3_after",
			"children": [
				{"kind":"t3","data":{"id":"post1","title":"First Post","subreddit":"golang","is_self":true,"score":42,"num_comments":5,"created_utc":1700000000}},
				{"kind":"t3","data":{"id":"post2","title":"Second Post","subreddit":"golang","is_self":false,"score":100,"num_comments":10,"created_utc":1700000100,"domain":"example.com","url":"https://example.com/article"}}
			]
		}
	}`
	posts, before, after, err := ParseSubredditListing([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("expected 2 posts, got %d", len(posts))
	}
	if before != "t3_before" {
		t.Errorf("before = %q, want t3_before", before)
	}
	if after != "t3_after" {
		t.Errorf("after = %q, want t3_after", after)
	}
	if posts[0].ID != "post1" || posts[0].Title != "First Post" {
		t.Errorf("post 0: id=%q title=%q", posts[0].ID, posts[0].Title)
	}
	if posts[1].ID != "post2" {
		t.Errorf("post 1 id = %q", posts[1].ID)
	}
}

func TestParseSubredditListing_SkipsNonT3(t *testing.T) {
	data := `{
		"kind": "Listing",
		"data": {
			"children": [
				{"kind":"t3","data":{"id":"post1","is_self":true,"score":1,"created_utc":1700000000}},
				{"kind":"t1","data":{"id":"comment1"}},
				{"kind":"t3","data":{"id":"post2","is_self":true,"score":2,"created_utc":1700000100}}
			]
		}
	}`
	posts, _, _, err := ParseSubredditListing([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(posts) != 2 {
		t.Errorf("expected 2 posts (skipping t1), got %d", len(posts))
	}
}

func TestParsePost_SelfPost(t *testing.T) {
	raw := json.RawMessage(`{"kind":"t3","data":{
		"id":"abc123","title":"My Self Post","subreddit":"test",
		"is_self":true,"score":42,"num_comments":7,
		"created_utc":1700000000,"author":"testuser",
		"permalink":"/r/test/comments/abc123/my_self_post/",
		"selftext_html":"<p>Hello world</p>"
	}}`)
	post, err := ParsePost(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.ID != "abc123" {
		t.Errorf("ID = %q", post.ID)
	}
	if post.PostType != "self" {
		t.Errorf("PostType = %q, want self", post.PostType)
	}
	if post.Author.Name != "testuser" {
		t.Errorf("Author = %q", post.Author.Name)
	}
	if post.Score[1] != "42" {
		t.Errorf("Score raw = %q, want 42", post.Score[1])
	}
}

// TestParsePost_SelfPostBodyImageEmbedded pins that an image pasted into a self
// post's selftext (the "Remove Turnkey footer" case) renders as an inline <img>
// rather than a bare /preview/pre/... link the reader has to click. Reddit emits
// it as an auto-link whose visible text equals its href; after RewriteURLs both
// are local proxy paths and EmbedCommentImages inlines it.
func TestParsePost_SelfPostBodyImageEmbedded(t *testing.T) {
	raw := json.RawMessage(`{"kind":"t3","data":{
		"id":"u9hwfp","title":"Remove Turnkey footer","subreddit":"selfhosted",
		"is_self":true,"score":2,"num_comments":4,
		"created_utc":1700000000,"author":"rapturedShadow",
		"permalink":"/r/selfhosted/comments/u9hwfp/remove_turnkey_footer/",
		"selftext_html":"<div class=\"md\"><p>footer:</p><p><a href=\"https://preview.redd.it/ahmk357bs38h1.png?width=370&amp;format=png&amp;auto=webp&amp;s=abc\">https://preview.redd.it/ahmk357bs38h1.png?width=370&amp;format=png&amp;auto=webp&amp;s=abc</a></p></div>"
	}}`)
	post, err := ParsePost(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := string(post.Body)
	if !strings.Contains(body, "<img") {
		t.Errorf("self-post body image should render as <img>, got: %q", body)
	}
	if !strings.Contains(body, `src="/preview/pre/ahmk357bs38h1.png?width=370&amp;format=png&amp;auto=webp&amp;s=abc"`) {
		t.Errorf("embedded <img> should point at the local proxy path, got: %q", body)
	}
}

func TestParsePost_LinkPost(t *testing.T) {
	raw := json.RawMessage(`{"kind":"t3","data":{
		"id":"def456","title":"Link Post","subreddit":"news",
		"is_self":false,"score":1000,"num_comments":50,
		"created_utc":1700000000,"domain":"example.com",
		"url":"https://example.com/article",
		"url_overridden_by_dest":"https://example.com/article"
	}}`)
	post, err := ParsePost(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.PostType != "link" {
		t.Errorf("PostType = %q, want link", post.PostType)
	}
	if post.OutURL == nil || *post.OutURL != "https://example.com/article" {
		t.Error("OutURL not set correctly")
	}
}

func TestParsePost_ImagePost(t *testing.T) {
	raw := json.RawMessage(`{"kind":"t3","data":{
		"id":"img1","title":"Image","subreddit":"pics",
		"is_self":false,"score":500,"num_comments":20,
		"created_utc":1700000000,"post_hint":"image",
		"domain":"i.redd.it","url":"https://i.redd.it/photo.jpg"
	}}`)
	post, err := ParsePost(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.PostType != "image" {
		t.Errorf("PostType = %q, want image", post.PostType)
	}
}

func TestParsePost_VideoPost(t *testing.T) {
	raw := json.RawMessage(`{"kind":"t3","data":{
		"id":"vid1","title":"Video","subreddit":"videos",
		"is_self":false,"score":200,"num_comments":15,
		"created_utc":1700000000,
		"secure_media":{"reddit_video":{
			"fallback_url":"https://v.redd.it/abc/DASH_720.mp4?source=fallback",
			"hls_url":"https://v.redd.it/abc/HLSPlaylist.m3u8",
			"width":1280,"height":720,"is_gif":false
		}}
	}}`)
	post, err := ParsePost(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.PostType != "video" {
		t.Errorf("PostType = %q, want video", post.PostType)
	}
	if post.Media.Width != 1280 {
		t.Errorf("Media.Width = %d, want 1280", post.Media.Width)
	}
}

func TestParsePost_GalleryPost(t *testing.T) {
	raw := json.RawMessage(`{"kind":"t3","data":{
		"id":"gal1","title":"Gallery","subreddit":"art",
		"is_self":false,"is_gallery":true,"score":300,"num_comments":8,
		"created_utc":1700000000,
		"gallery_data":{"items":[
			{"media_id":"m1","caption":"First"},
			{"media_id":"m2","caption":"Second"}
		]},
		"media_metadata":{
			"m1":{"m":"image/png","s":{"u":"https://preview.redd.it/m1.png","x":800,"y":600}},
			"m2":{"m":"image/jpeg","s":{"u":"https://preview.redd.it/m2.jpg","x":1024,"y":768}}
		}
	}}`)
	post, err := ParsePost(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.PostType != "gallery" {
		t.Errorf("PostType = %q, want gallery", post.PostType)
	}
	if len(post.Gallery) != 2 {
		t.Fatalf("Gallery len = %d, want 2", len(post.Gallery))
	}
	if post.Gallery[0].Caption != "First" {
		t.Errorf("Gallery[0].Caption = %q", post.Gallery[0].Caption)
	}
	if post.Gallery[1].Width != 1024 {
		t.Errorf("Gallery[1].Width = %d, want 1024", post.Gallery[1].Width)
	}
}

func TestParsePost_HiddenScore(t *testing.T) {
	raw := json.RawMessage(`{"kind":"t3","data":{
		"id":"hs1","is_self":true,"score":99,"hide_score":true,"created_utc":1700000000
	}}`)
	post, err := ParsePost(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.Score[0] != "•" {
		t.Errorf("hidden score display = %q, want •", post.Score[0])
	}
}

func TestParsePost_Flags(t *testing.T) {
	raw := json.RawMessage(`{"kind":"t3","data":{
		"id":"fl1","is_self":true,"score":1,"created_utc":1700000000,
		"spoiler":true,"over_18":true,"stickied":true
	}}`)
	post, err := ParsePost(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !post.Flags.Spoiler {
		t.Error("expected Spoiler flag")
	}
	if !post.Flags.NSFW {
		t.Error("expected NSFW flag")
	}
	if !post.Flags.Stickied {
		t.Error("expected Stickied flag")
	}
}

func TestParsePost_InvalidKind(t *testing.T) {
	raw := json.RawMessage(`{"kind":"t1","data":{"id":"c1"}}`)
	_, err := ParsePost(raw)
	if err == nil {
		t.Error("expected error for non-t3 kind")
	}
}

func TestParsePostPage(t *testing.T) {
	data := `[
		{"kind":"Listing","data":{"children":[
			{"kind":"t3","data":{"id":"p1","title":"Test Post","subreddit":"test","is_self":true,"score":10,"num_comments":2,"created_utc":1700000000,"permalink":"/r/test/comments/p1/test_post/","author":"op"}}
		]}},
		{"kind":"Listing","data":{"children":[
			{"kind":"t1","data":{"id":"c1","body_html":"<p>Comment 1</p>","author":"user1","score":5,"created_utc":1700000100,"parent_id":"t3_p1","replies":""}},
			{"kind":"t1","data":{"id":"c2","body_html":"<p>Comment 2</p>","author":"user2","score":3,"created_utc":1700000200,"parent_id":"t3_p1","replies":""}}
		]}}
	]`
	post, comments, err := ParsePostPage([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.ID != "p1" {
		t.Errorf("post ID = %q", post.ID)
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(comments))
	}
	if comments[0].ID != "c1" {
		t.Errorf("comment 0 ID = %q", comments[0].ID)
	}
}

func TestParsePostPage_InvalidJSON(t *testing.T) {
	_, _, err := ParsePostPage([]byte(`not json`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParsePostPage_TooFewListings(t *testing.T) {
	_, _, err := ParsePostPage([]byte(`[{"kind":"Listing","data":{"children":[]}}]`))
	if err == nil {
		t.Error("expected error for single listing")
	}
}

func TestParseComments_RepliesEmptyString(t *testing.T) {
	data := json.RawMessage(`{"kind":"Listing","data":{"children":[
		{"kind":"t1","data":{"id":"c1","body_html":"test","author":"u","score":1,"created_utc":1700000000,"parent_id":"t3_p1","replies":""}}
	]}}`)
	comments := ParseComments(data, "/r/test/comments/p1/", "op")
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if len(comments[0].Replies) != 0 {
		t.Errorf("expected 0 replies for empty string, got %d", len(comments[0].Replies))
	}
}

func TestParseComments_NestedReplies(t *testing.T) {
	data := json.RawMessage(`{"kind":"Listing","data":{"children":[
		{"kind":"t1","data":{
			"id":"c1","body_html":"top","author":"u1","score":10,"created_utc":1700000000,"parent_id":"t3_p1",
			"replies":{"kind":"Listing","data":{"children":[
				{"kind":"t1","data":{
					"id":"c2","body_html":"nested","author":"u2","score":5,"created_utc":1700000100,"parent_id":"t1_c1",
					"replies":{"kind":"Listing","data":{"children":[
						{"kind":"t1","data":{"id":"c3","body_html":"deep","author":"u3","score":1,"created_utc":1700000200,"parent_id":"t1_c2","replies":""}}
					]}}
				}}
			]}}
		}}
	]}}`)
	comments := ParseComments(data, "/r/test/comments/p1/", "op")
	if len(comments) != 1 {
		t.Fatalf("expected 1 top-level comment, got %d", len(comments))
	}
	if len(comments[0].Replies) != 1 {
		t.Fatalf("expected 1 reply to c1, got %d", len(comments[0].Replies))
	}
	if comments[0].Replies[0].ID != "c2" {
		t.Errorf("reply ID = %q, want c2", comments[0].Replies[0].ID)
	}
	if len(comments[0].Replies[0].Replies) != 1 {
		t.Fatalf("expected 1 reply to c2, got %d", len(comments[0].Replies[0].Replies))
	}
	if comments[0].Replies[0].Replies[0].ID != "c3" {
		t.Errorf("deep reply ID = %q, want c3", comments[0].Replies[0].Replies[0].ID)
	}
}

func TestParseComments_MoreKind(t *testing.T) {
	data := json.RawMessage(`{"kind":"Listing","data":{"children":[
		{"kind":"more","data":{"id":"_","count":42,"parent_id":"t1_abc"}}
	]}}`)
	comments := ParseComments(data, "/r/test/comments/p1/", "op")
	if len(comments) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(comments))
	}
	if comments[0].Kind != "more" {
		t.Errorf("Kind = %q, want more", comments[0].Kind)
	}
	if comments[0].MoreCount != 42 {
		t.Errorf("MoreCount = %d, want 42", comments[0].MoreCount)
	}
}

func TestParseComments_InvalidJSON(t *testing.T) {
	comments := ParseComments(json.RawMessage(`not json`), "", "")
	if len(comments) != 0 {
		t.Errorf("expected 0 comments for invalid JSON, got %d", len(comments))
	}
}

func TestParseSubredditAbout(t *testing.T) {
	data := `{"kind":"t5","data":{
		"display_name":"golang","title":"The Go Programming Language",
		"public_description":"For discussion about Go",
		"subscribers":250000,"accounts_active":1200,
		"over18":false,"wiki_enabled":true,
		"description_html":"<p>Welcome to r/golang</p>",
		"community_icon":"https://styles.redditmedia.com/icon.png"
	}}`
	sub, err := ParseSubredditAbout([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub.Name != "golang" {
		t.Errorf("Name = %q", sub.Name)
	}
	if sub.Title != "The Go Programming Language" {
		t.Errorf("Title = %q", sub.Title)
	}
	if sub.Members[1] != "250000" {
		t.Errorf("Members raw = %q", sub.Members[1])
	}
	if !sub.Wiki {
		t.Error("expected Wiki=true")
	}
}

func TestParsePost_Flair(t *testing.T) {
	raw := json.RawMessage(`{"kind":"t3","data":{
		"id":"fl1","is_self":true,"score":1,"created_utc":1700000000,
		"link_flair_text":"Discussion","link_flair_type":"text",
		"link_flair_background_color":"#ff4500","link_flair_text_color":"light"
	}}`)
	post, err := ParsePost(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.Flair.Text != "Discussion" {
		t.Errorf("Flair.Text = %q", post.Flair.Text)
	}
	if post.Flair.BackgroundColor != "#ff4500" {
		t.Errorf("Flair.BackgroundColor = %q", post.Flair.BackgroundColor)
	}
}

func TestParsePost_Awards(t *testing.T) {
	raw := json.RawMessage(`{"kind":"t3","data":{
		"id":"aw1","is_self":true,"score":1,"created_utc":1700000000,
		"all_awardings":[
			{"name":"Gold","description":"Shiny","count":3,"resized_icons":[{"url":"https://www.redditstatic.com/gold.png","width":16,"height":16}]}
		]
	}}`)
	post, err := ParsePost(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(post.Awards) != 1 {
		t.Fatalf("expected 1 award, got %d", len(post.Awards))
	}
	if post.Awards[0].Name != "Gold" {
		t.Errorf("Award name = %q", post.Awards[0].Name)
	}
	if post.Awards[0].Count != 3 {
		t.Errorf("Award count = %d", post.Awards[0].Count)
	}
}

// Post removed/deleted detection is what the archive layer keys off to skip
// overwriting a previously-good local copy. Cover the four signals Reddit
// uses in the wild: removed_by_category, selftext sentinel, deleted-author
// self-post with empty body, and the "still alive" baseline.
func TestParsePost_RemovedDetection(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{
			name: "removed_by_category=moderator flips Removed",
			data: `{"kind":"t3","data":{"id":"p1","title":"x","is_self":true,"removed_by_category":"moderator","created_utc":1700000000}}`,
			want: true,
		},
		{
			name: "removed_by_category=deleted flips Removed",
			data: `{"kind":"t3","data":{"id":"p2","title":"x","is_self":true,"removed_by_category":"deleted","created_utc":1700000000}}`,
			want: true,
		},
		{
			name: "selftext=[removed] without category flips Removed",
			data: `{"kind":"t3","data":{"id":"p3","title":"x","is_self":true,"selftext":"[removed]","created_utc":1700000000}}`,
			want: true,
		},
		{
			name: "selftext=[deleted] flips Removed",
			data: `{"kind":"t3","data":{"id":"p4","title":"x","is_self":true,"selftext":"[deleted]","created_utc":1700000000}}`,
			want: true,
		},
		{
			name: "author=[deleted] self-post with empty body flips Removed",
			data: `{"kind":"t3","data":{"id":"p5","title":"x","is_self":true,"author":"[deleted]","selftext":"","created_utc":1700000000}}`,
			want: true,
		},
		{
			name: "alive post stays Removed=false",
			data: `{"kind":"t3","data":{"id":"p6","title":"x","is_self":true,"selftext":"hello world","author":"alice","created_utc":1700000000}}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			post, err := ParsePost(json.RawMessage(tc.data))
			if err != nil {
				t.Fatalf("ParsePost: %v", err)
			}
			if post.Removed != tc.want {
				t.Errorf("Removed = %v, want %v", post.Removed, tc.want)
			}
		})
	}
}

// Comment removal: the two sentinel-body branches must set Comment.Removed so
// the comment renderer can attach a Time Machine badge without making a second
// pass over the body string.
func TestParseComments_RemovedDetection(t *testing.T) {
	data := []byte(`{"kind":"Listing","data":{"children":[
		{"kind":"t1","data":{"id":"c1","author":"[deleted]","body_html":"&lt;p&gt;[removed]&lt;/p&gt;","created_utc":1700000000}},
		{"kind":"t1","data":{"id":"c2","author":"someone","body_html":"&lt;p&gt;[ Removed by Reddit ]&lt;/p&gt;","created_utc":1700000000}},
		{"kind":"t1","data":{"id":"c3","author":"alice","body_html":"&lt;p&gt;hello&lt;/p&gt;","created_utc":1700000000}}
	]}}`)
	comments := ParseComments(data, "/r/test/comments/abc/", "op")
	if len(comments) != 3 {
		t.Fatalf("expected 3 comments, got %d", len(comments))
	}
	if !comments[0].Removed {
		t.Errorf("c1 (deleted+[removed] body) Removed=false")
	}
	if !comments[1].Removed {
		t.Errorf("c2 ([ Removed by Reddit ] body) Removed=false")
	}
	if comments[2].Removed {
		t.Errorf("c3 (normal comment) Removed=true")
	}
}
