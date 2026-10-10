// Package linkcheck checks a batch of item URLs and marks links dead after
// three consecutive hard failures.
package linkcheck

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"syscall"

	"golang.org/x/sync/errgroup"

	"dogear/internal/db"
	"dogear/internal/safety"
)

// Outcome of one check.
type Outcome int

const (
	OK      Outcome = iota // 2xx/3xx
	Fail                   // 404, 410, NXDOMAIN, connection refused
	Unknown                // 403, 429, 5xx, timeouts: no change
)

const deadAfter = 3

type Checker struct {
	Q      *db.Queries
	Client *http.Client
	Log    *slog.Logger
	Batch  int
}

type Summary struct{ Checked, OK, Failed, Unknown, Dead int }

// Run checks up to Batch items, oldest-checked first, with 20 requests in
// flight overall and one at a time per host.
func (c *Checker) Run(ctx context.Context) (Summary, error) {
	batch := c.Batch
	if batch <= 0 {
		batch = 2000
	}
	rows, err := c.Q.ItemsToLinkCheck(ctx, int32(batch))
	if err != nil {
		return Summary{}, err
	}
	var (
		mu    sync.Mutex
		sum   Summary
		hosts sync.Map // host -> *sync.Mutex
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(20)
	for _, row := range rows {
		g.Go(func() error {
			host := ""
			if u, err := url.Parse(row.Url); err == nil {
				host = u.Hostname()
			}
			lock, _ := hosts.LoadOrStore(host, &sync.Mutex{})
			lock.(*sync.Mutex).Lock()
			out := Check(gctx, c.Client, row.Url)
			lock.(*sync.Mutex).Unlock()

			status, fails := "unknown", row.LinkFailCount
			switch out {
			case OK:
				status, fails = "ok", 0
			case Fail:
				fails++
				status = "unknown"
				if fails >= deadAfter {
					status = "dead"
				}
			case Unknown:
				// Leave the status and counter alone; only the check time moves.
				if row.LinkFailCount >= deadAfter {
					status = "dead"
				}
			}
			if err := c.Q.SetLinkResult(gctx, db.SetLinkResultParams{ID: row.ID, LinkStatus: status, LinkFailCount: fails}); err != nil {
				return err
			}
			mu.Lock()
			sum.Checked++
			switch out {
			case OK:
				sum.OK++
			case Fail:
				sum.Failed++
			default:
				sum.Unknown++
			}
			if status == "dead" {
				sum.Dead++
			}
			mu.Unlock()
			return nil
		})
	}
	err = g.Wait()
	return sum, err
}

// Check tries HEAD first and confirms anything other than success with a
// GET (body capped at 64 KB), since some servers mishandle HEAD.
func Check(ctx context.Context, client *http.Client, u string) Outcome {
	if do(ctx, client, http.MethodHead, u) == OK {
		return OK
	}
	return do(ctx, client, http.MethodGet, u)
}

func do(ctx context.Context, client *http.Client, method, u string) Outcome {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return Fail
	}
	resp, err := client.Do(req)
	if err != nil {
		return classifyErr(err)
	}
	defer resp.Body.Close()
	if method == http.MethodGet {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	}
	switch {
	case resp.StatusCode < 400:
		return OK
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return Fail
	default:
		return Unknown
	}
}

// classifyErr maps transport errors: DNS NXDOMAIN and connection refused are
// hard failures; timeouts, blocked addresses, and everything else are unknown.
func classifyErr(err error) Outcome {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return Fail
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return Fail
	}
	if errors.Is(err, safety.ErrBlockedAddress) {
		return Unknown
	}
	return Unknown
}
