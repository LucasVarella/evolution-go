package chatwoot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewClientRejectsBadURL(t *testing.T) {
	for _, u := range []string{"", "chatwoot-rails:3000", "/relative"} {
		if _, err := NewClient(u, 1, "t"); err == nil {
			t.Errorf("NewClient(%q) accepted an invalid URL", u)
		}
	}
}

func TestInternalURL(t *testing.T) {
	c, _ := NewClient("http://chatwoot-rails:3000", 1, "t")
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"http://147.93.11.77:3000/rails/active_storage/blobs/redirect/abc/file.pdf?x=1", "http://chatwoot-rails:3000/rails/active_storage/blobs/redirect/abc/file.pdf?x=1", true},
		{"/rails/active_storage/blobs/redirect/abc/file.pdf", "http://chatwoot-rails:3000/rails/active_storage/blobs/redirect/abc/file.pdf", true},
		{"http://169.254.169.254/latest/meta-data", "", false},
		{"http://evil.example/rails/../api/v1/profile", "", false},
		{"http://x/rails/active_storage/../../api/v1/profile", "", false},
		{"file:///etc/passwd", "", false},
	}
	for _, c2 := range cases {
		got, err := c.internalURL(c2.in)
		if c2.ok != (err == nil) {
			t.Errorf("internalURL(%q) err = %v", c2.in, err)
			continue
		}
		if c2.ok && got.String() != c2.want {
			t.Errorf("internalURL(%q) = %q, want %q", c2.in, got, c2.want)
		}
	}
}

func TestDownloadDoesNotSendTokenToOtherHosts(t *testing.T) {
	var gotToken string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("api_access_token")
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF"))
	}))
	defer storage.Close()
	cw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storage.URL+"/bucket/file.pdf?signature=x", http.StatusFound) // like S3 storage
	}))
	defer cw.Close()

	c, _ := NewClient(cw.URL, 1, "secret-token")
	file, err := c.Download(context.Background(), "http://public:3000/rails/active_storage/blobs/redirect/sig/file.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if gotToken != "" {
		t.Fatalf("token sent to storage host: %q", gotToken)
	}
	if string(file.Data) != "%PDF" || file.ContentType != "application/pdf" || file.Filename != "file.pdf" {
		t.Fatalf("unexpected file %+v", file)
	}
}

func TestAPIErrorAndNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("api_access_token") != "tok" {
			t.Errorf("missing token header")
		}
		http.Error(w, `{"error":"Resource could not be found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, 3, "tok")
	_, err := c.CreateMessage(context.Background(), 5, NewMessage{Content: "x", MessageType: "incoming"})
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound(%v) = false", err)
	}
	if !strings.Contains(err.Error(), "/api/v1/accounts/3/conversations/5/messages") {
		t.Errorf("error lacks the path: %v", err)
	}
}

func TestMaskToken(t *testing.T) {
	if got := maskToken("abcdefgh"); got != "abcd…" {
		t.Errorf("maskToken = %q", got)
	}
	if got := maskToken("abc"); got != "" {
		t.Errorf("short token not hidden: %q", got)
	}
}
