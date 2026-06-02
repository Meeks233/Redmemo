package render

import "testing"

func TestWithDownloadTitle(t *testing.T) {
	tests := []struct {
		name, url, title, want string
	}{
		{
			name: "appends with ? when no query",
			url:  "https://media.example.com/x.mp4",
			title: "hello world",
			want: "https://media.example.com/x.mp4?dl_title=hello%20world",
		},
		{
			name: "appends with & when query present",
			url:  "https://media.example.com/x.mp4?sig=abc",
			title: "name",
			want: "https://media.example.com/x.mp4?sig=abc&dl_title=name",
		},
		{
			name: "percent-encodes unsafe bytes",
			url:  "https://e.com/v",
			title: "a/b+c?d&e",
			want: "https://e.com/v?dl_title=a%2Fb%2Bc%3Fd%26e",
		},
		{
			name: "keeps unreserved characters",
			url:  "https://e.com/v",
			title: "A-z_0.9~",
			want: "https://e.com/v?dl_title=A-z_0.9~",
		},
		{
			name: "no-op when title empty",
			url:  "https://e.com/v?x=1",
			title: "",
			want: "https://e.com/v?x=1",
		},
		{
			name: "no-op when url empty",
			url:  "",
			title: "name",
			want: "",
		},
		{
			name: "no-op when dl_title already present",
			url:  "https://e.com/v?dl_title=already",
			title: "new",
			want: "https://e.com/v?dl_title=already",
		},
		{
			name: "no-op when fragment present",
			url:  "https://e.com/v#frag",
			title: "name",
			want: "https://e.com/v#frag",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := WithDownloadTitle(tc.url, tc.title)
			if got != tc.want {
				t.Fatalf("WithDownloadTitle(%q, %q) = %q, want %q", tc.url, tc.title, got, tc.want)
			}
		})
	}
}

func TestQueryEscape(t *testing.T) {
	// Sanity: every byte 0..255 either passes through unchanged (unreserved)
	// or becomes a 3-byte %HH sequence. Output must always be ASCII-printable.
	for b := 0; b < 256; b++ {
		s := string([]byte{byte(b)})
		out := queryEscape(s)
		if len(out) != 1 && len(out) != 3 {
			t.Fatalf("byte %d: unexpected length %d (%q)", b, len(out), out)
		}
		if len(out) == 3 && out[0] != '%' {
			t.Fatalf("byte %d: expected percent-encoded, got %q", b, out)
		}
	}
}
