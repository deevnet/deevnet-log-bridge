package bridge

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// TenantHeader is how this bridge tells the proxy which tenant a batch belongs
// to. The proxy matches it against the routes the Deevnet API wrote, then
// overwrites the store's own partition headers with that route's values and
// removes this one (ADR-0027, open question 1; CHG-0020).
//
// So this header SELECTS among tenants the API has written. It cannot invent a
// partition, and a tenant the API has never created has no route at all - a
// batch for one is refused rather than landing somewhere.
const TenantHeader = "X-Deevnet-Tenant"

// Shipper posts batches to the log store.
type Shipper struct {
	URL   string
	Token string

	Client *http.Client
	Log    *slog.Logger

	// Retries bound how long one batch may hold the queue. Logs are not
	// authoritative data (ADR-0022 §6): losing a batch is bad, and blocking
	// every other tenant's logs behind it for ever is worse.
	Retries int
	Backoff time.Duration
}

// NewShipper builds a shipper that trusts the site CA and nothing else.
func NewShipper(url, token, caFile string, log *slog.Logger) (*Shipper, error) {
	if url == "" || token == "" {
		return nil, fmt.Errorf("the store's URL and token are both required")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading the site CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("the site CA at %s holds no certificate", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &Shipper{
		URL:   strings.TrimRight(url, "/") + "/insert/jsonline",
		Token: token,
		Client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
		Log:     log,
		Retries: 3,
		Backoff: 2 * time.Second,
	}, nil
}

// Send posts one tenant's batch, retrying a failure that may be transient.
func (s *Shipper) Send(ctx context.Context, b Batch) error {
	body, err := b.Body()
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt <= s.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.Backoff * time.Duration(attempt)):
			}
		}
		err := s.post(ctx, b.Tenant, body)
		if err == nil {
			return nil
		}
		last = err
		// A refusal is not retried: the same request will be refused again.
		// Only an unreachable or erroring store is worth another attempt.
		if isRefusal(err) {
			return err
		}
	}
	return last
}

// refusal marks an answer that will not improve by being sent again.
type refusal struct{ error }

func isRefusal(err error) bool {
	var r refusal
	return errors.As(err, &r)
}

func (s *Shipper) post(ctx context.Context, tenant string, body []byte) error {
	// _stream_fields keeps each device's lines in their own stream, which is
	// what makes a per-device view cheap in the store.
	url := s.URL + "?_stream_fields=tenant,device&_time_field=_time&_msg_field=_msg"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/stream+json")
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set(TenantHeader, tenant)

	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("posting to the store: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	switch {
	case resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusBadRequest:
		// 401 is this bridge's own token; 400 is a route the proxy does not
		// have, which is what a tenant the API has never created looks like.
		// Neither improves by being sent again.
		return refusal{fmt.Errorf("the store refused a batch for %q: %s", tenant, resp.Status)}
	default:
		return fmt.Errorf("the store answered %s for %q", resp.Status, tenant)
	}
}

// Queue collects lines and ships them in batches, one tenant per request.
//
// It is bounded. A device that talks faster than the store accepts must not
// grow this process until it is killed, and it must not be able to stop other
// tenants' logs by filling the queue: when the queue is full the OLDEST lines
// are dropped and counted, so what survives is the most recent picture.
type Queue struct {
	mu      sync.Mutex
	lines   []Line
	dropped int

	Max     int
	Flush   time.Duration
	Shipper *Shipper
	Log     *slog.Logger
}

func NewQueue(s *Shipper, max int, flush time.Duration, log *slog.Logger) *Queue {
	return &Queue{Max: max, Flush: flush, Shipper: s, Log: log}
}

// Add queues a line, dropping the oldest if the queue is full.
func (q *Queue) Add(l Line) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.lines) >= q.Max {
		q.lines = q.lines[1:]
		q.dropped++
	}
	q.lines = append(q.lines, l)
}

// Run ships until the context is cancelled, then makes one last attempt so a
// clean stop does not throw away what is already in hand.
func (q *Queue) Run(ctx context.Context) {
	t := time.NewTicker(q.Flush)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			q.flush(flushCtx)
			cancel()
			return
		case <-t.C:
			q.flush(ctx)
		}
	}
}

func (q *Queue) flush(ctx context.Context) {
	q.mu.Lock()
	lines := q.lines
	dropped := q.dropped
	q.lines, q.dropped = nil, 0
	q.mu.Unlock()

	if dropped > 0 {
		// Said out loud, every time. Silent loss is the failure mode a log
		// pipeline must not have.
		q.Log.Warn("queue full; oldest lines dropped", "dropped", dropped)
	}
	if len(lines) == 0 {
		return
	}
	for _, b := range Group(lines) {
		if err := q.Shipper.Send(ctx, b); err != nil {
			q.Log.Error("shipping a batch", "tenant", b.Tenant, "lines", len(b.Lines), "err", err)
		}
	}
}
