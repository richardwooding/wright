package websearch_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/websearch"
)

func TestNewSelectsAProvider(t *testing.T) {
	tests := []struct {
		name, provider, key string
		wantErr             error
	}{
		{name: "unconfigured is not a failure", wantErr: websearch.ErrNoProvider},
		{name: "configured without a key", provider: "brave", wantErr: websearch.ErrNoKey},
		{name: "unknown provider", provider: "altavista", key: "k", wantErr: websearch.ErrUnknown},
		{name: "brave", provider: "brave", key: "k"},
		{name: "case and spacing are forgiven", provider: " Brave ", key: "k"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := websearch.New(websearch.Config{Name: tt.provider, Key: tt.key})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && p == nil {
				t.Fatal("no provider returned")
			}
		})
	}
	if got := websearch.KeyEnv("brave"); got != "BRAVE_API_KEY" {
		t.Errorf("KeyEnv = %q", got)
	}
}

func TestBraveSearch(t *testing.T) {
	var gotQuery, gotCount, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		gotCount = r.URL.Query().Get("count")
		gotToken = r.Header.Get("X-Subscription-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[
			{"title":"One","url":"https://example.test/1","description":"first"},
			{"title":"Two","url":"https://example.test/2","description":"second"},
			{"title":"Three","url":"https://example.test/3","description":"third"}]}}`))
	}))
	defer srv.Close()

	p, err := websearch.New(websearch.Config{Name: "brave", Key: "secret-key", BaseURL: srv.URL, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	// The limit is honoured locally too: a provider that ignores count must
	// not be able to flood the model's context.
	got, err := p.Search(context.Background(), "iter.Seq2", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(got), got)
	}
	if got[0].Title != "One" || got[0].URL != "https://example.test/1" || got[0].Snippet != "first" {
		t.Errorf("first result = %+v", got[0])
	}
	if gotQuery != "iter.Seq2" || gotCount != "2" || gotToken != "secret-key" {
		t.Errorf("request = q:%q count:%q token:%q", gotQuery, gotCount, gotToken)
	}
}

func TestBraveErrors(t *testing.T) {
	t.Run("a non-200 is reported without echoing the key", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
		}))
		defer srv.Close()
		p, _ := websearch.New(websearch.Config{Name: "brave", Key: "secret-key", BaseURL: srv.URL, Client: srv.Client()})
		_, err := p.Search(context.Background(), "x", 5)
		if err == nil {
			t.Fatal("want an error")
		}
		if !strings.Contains(err.Error(), "rate limited") {
			t.Errorf("error does not carry the provider's reason: %v", err)
		}
		if strings.Contains(err.Error(), "secret-key") {
			t.Errorf("the API key leaked into an error: %v", err)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("{not json"))
		}))
		defer srv.Close()
		p, _ := websearch.New(websearch.Config{Name: "brave", Key: "k", BaseURL: srv.URL, Client: srv.Client()})
		if _, err := p.Search(context.Background(), "x", 5); err == nil {
			t.Fatal("want a decode error")
		}
	})
}
