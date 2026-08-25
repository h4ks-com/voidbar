package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const cdxEndpoint = "https://web.archive.org/cdx/search/cdx"

// SplitWayback decomposes a Wayback "id_" URL
// (https://web.archive.org/web/<timestamp>id_/<original>) into the original
// URL. ok is false for any other base.
func SplitWayback(base string) (original string, ok bool) {
	for _, prefix := range []string{"https://web.archive.org/web/", "http://web.archive.org/web/"} {
		if !strings.HasPrefix(base, prefix) {
			continue
		}
		rest := base[len(prefix):]
		slash := strings.Index(rest, "/")
		if slash <= 0 {
			return "", false
		}
		ts := rest[:slash]
		orig := rest[slash+1:]
		if !strings.HasSuffix(ts, "id_") {
			return "", false
		}
		if !strings.Contains(orig, "://") {
			return "", false
		}
		return orig, true
	}
	return "", false
}

// BuildWaybackIDURL builds a raw-content Wayback URL for the given snapshot
// timestamp and original URL.
func BuildWaybackIDURL(timestamp, original string) string {
	return "https://web.archive.org/web/" + timestamp + "id_/" + original
}

type cdxRow struct {
	Timestamp  string
	Original   string
	StatusCode string
}

// parseCDXResponse parses the JSON output of the CDX API. The first row is
// the header; only rows with status code 200 are returned.
func parseCDXResponse(body []byte) ([]cdxRow, error) {
	var raw [][]string
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty cdx response")
	}
	header := raw[0]
	idx := map[string]int{}
	for i, h := range header {
		idx[h] = i
	}
	ti, okT := idx["timestamp"]
	si, okS := idx["statuscode"]
	oi, _ := idx["original"]
	if !okT || !okS {
		return nil, fmt.Errorf("unexpected cdx header %v", header)
	}
	var rows []cdxRow
	for _, r := range raw[1:] {
		if len(r) <= ti || len(r) <= si {
			continue
		}
		if r[si] != "200" {
			continue
		}
		row := cdxRow{Timestamp: r[ti], StatusCode: r[si]}
		if len(r) > oi {
			row.Original = r[oi]
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// CDXLookup finds an archived 200 snapshot of originalURL via the Wayback
// CDX API and returns a raw-content (id_) URL for it. The CDX endpoints
// rate-limit aggressively (429); lookups retry with backoff so a burst of
// missing files degrades to slow instead of being misreported as
// "never archived".
func CDXLookup(ctx context.Context, client *http.Client, originalURL string) (string, error) {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	q := url.Values{}
	q.Set("url", originalURL)
	q.Set("output", "json")
	q.Set("filter", "statuscode:200")
	q.Set("collapse", "digest")
	q.Set("limit", "5")

	backoff := 2 * time.Second
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cdxEndpoint+"?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "voidbar-mirror/0.1")
		res, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if res.StatusCode == http.StatusTooManyRequests {
			res.Body.Close()
			lastErr = fmt.Errorf("cdx http 429")
			continue
		}
		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			return "", fmt.Errorf("cdx http %d", res.StatusCode)
		}
		var buf strings.Builder
		if _, err := func() (int, error) {
			b := make([]byte, 32*1024)
			total := 0
			for {
				n, err := res.Body.Read(b)
				buf.Write(b[:n])
				total += n
				if err != nil || total > 1<<20 {
					return total, nil
				}
			}
		}(); err != nil {
			res.Body.Close()
			return "", err
		}
		res.Body.Close()
		rows, err := parseCDXResponse([]byte(buf.String()))
		if err != nil {
			return "", err
		}
		if len(rows) == 0 {
			return "", fmt.Errorf("no 200 snapshot for %s", originalURL)
		}
		return BuildWaybackIDURL(rows[0].Timestamp, rows[0].Original), nil
	}
	return "", lastErr
}
