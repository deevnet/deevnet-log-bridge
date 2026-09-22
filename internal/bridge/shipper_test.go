package bridge

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func shipperTo(t *testing.T, h http.HandlerFunc) (*Shipper, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s, err := NewShipper(srv.URL, "a-token", "", quiet())
	if err != nil {
		t.Fatalf("shipper: %v", err)
	}
	s.Backoff = time.Millisecond
	return s, srv
}

// The tenant travels in the header the proxy matches. Without it a batch would
// land in whatever partition the token's default route points at.
func TestSendNamesTheTenantInTheHeader(t *testing.T) {
	var gotTenant, gotAuth, gotBody string
	s, _ := shipperTo(t, func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get(TenantHeader)
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	})

	err := s.Send(context.Background(), Batch{Tenant: "eds", Lines: []Line{
		{Tenant: "eds", Fields: map[string]any{FieldMsg: "hello"}},
	}})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotTenant != "eds" {
		t.Errorf("%s = %q, want eds", TenantHeader, gotTenant)
	}
	if gotAuth != "Bearer a-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"hello"`) {
		t.Errorf("body = %q", gotBody)
	}
}

// A refusal is the store saying this request is wrong: the same bytes will be
// refused again, and retrying only delays every other tenant's logs.
func TestSendDoesNotRetryARefusal(t *testing.T) {
	var calls atomic.Int32
	s, _ := shipperTo(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})
	err := s.Send(context.Background(), Batch{Tenant: "nosuch", Lines: []Line{
		{Tenant: "nosuch", Fields: map[string]any{FieldMsg: "x"}},
	}})
	if err == nil {
		t.Fatal("a refused batch reported success")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d attempts, want 1", n)
	}
}

// A store that is erroring may simply be restarting, so this one is retried.
func TestSendRetriesAServerError(t *testing.T) {
	var calls atomic.Int32
	s, _ := shipperTo(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := s.Send(context.Background(), Batch{Tenant: "eds", Lines: []Line{
		{Tenant: "eds", Fields: map[string]any{FieldMsg: "x"}},
	}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("%d attempts, want 3", n)
	}
}

// One device shouting must not push every other tenant's logs out of memory,
// and the loss must be counted rather than silent.
func TestQueueDropsTheOldestWhenFull(t *testing.T) {
	var seen atomic.Int32
	s, _ := shipperTo(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.Add(int32(strings.Count(strings.TrimSpace(string(b)), "\n") + 1))
		w.WriteHeader(http.StatusNoContent)
	})
	q := NewQueue(s, 2, time.Hour, quiet())
	for i := range 5 {
		q.Add(Line{Tenant: "eds", Fields: map[string]any{FieldMsg: i}})
	}
	q.flush(context.Background())
	if n := seen.Load(); n != 2 {
		t.Fatalf("%d lines shipped, want the 2 the queue holds", n)
	}
}
