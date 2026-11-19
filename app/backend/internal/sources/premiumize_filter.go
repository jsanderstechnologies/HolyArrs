package sources

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/HolyArrs/holyarrs/app/backend/internal/core/database"
	domain "github.com/HolyArrs/holyarrs/app/backend/internal/core/model"
	"github.com/HolyArrs/holyarrs/app/backend/internal/core/outbound"
)

const pmCacheBatchSize = 50

// filterTorrentioByPremiumize batch-checks Torrentio results against the
// complete Premiumize.me cloud cache (/api/cache/check) and returns only the
// items that are cached and ready for instant .strm creation.
// If no Premiumize.me downloader is configured, items are returned unchanged.
func filterTorrentioByPremiumize(ctx context.Context, store *database.Store, secrets SecretReader, items []Item) []Item {
	if len(items) == 0 || store == nil || secrets == nil {
		return items
	}
	dls, err := database.Select[domain.Downloader](ctx, store.Read(), "downloaders", "adapter = 'premiumize' AND removed = 0 AND state != 'disabled' ORDER BY priority, id LIMIT 1")
	if err != nil || len(dls) == 0 || dls[0].SecretRef == nil {
		return items
	}
	apiKey, err := secrets.Get(ctx, *dls[0].SecretRef)
	if err != nil || strings.TrimSpace(apiKey) == "" {
		return items
	}
	base := strings.TrimRight(strings.TrimSpace(dls[0].URL), "/")
	if base == "" {
		base = "https://www.premiumize.me"
	}
	client := pmHTTPClient(dls[0].AllowLocal)
	cached, ok := checkPMCloudCache(ctx, client, base, strings.TrimSpace(apiKey), items)
	if !ok {
		return items
	}
	out := make([]Item, 0, len(items))
	for _, it := range items {
		h := strings.ToLower(strings.TrimSpace(it.InfoHash))
		if h != "" && cached[h] {
			out = append(out, it)
		}
	}
	return out
}

func pmHTTPClient(allowLocal bool) *http.Client {
	allow := outbound.PublicOnly
	if allowLocal {
		allow = outbound.NotMetadata
	}
	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport(Config{AllowAddr: allow}),
	}
}

func checkPMCloudCache(ctx context.Context, hc *http.Client, base, apiKey string, items []Item) (map[string]bool, bool) {
	seen := map[string]bool{}
	var hashes []string
	for _, it := range items {
		h := strings.ToLower(strings.TrimSpace(it.InfoHash))
		if h != "" && !seen[h] {
			seen[h] = true
			hashes = append(hashes, h)
		}
	}
	if len(hashes) == 0 {
		return map[string]bool{}, true
	}

	cached := map[string]bool{}
	anyOK := false
	for start := 0; start < len(hashes); start += pmCacheBatchSize {
		end := start + pmCacheBatchSize
		if end > len(hashes) {
			end = len(hashes)
		}
		batch := hashes[start:end]
		q := url.Values{}
		q.Set("apikey", apiKey)
		for _, h := range batch {
			q.Add("items[]", h)
		}
		var res struct {
			Status   string `json:"status"`
			Response []bool `json:"response"`
		}
		if err := pmGetJSON(ctx, hc, base+"/api/cache/check", q, &res); err != nil {
			continue
		}
		if res.Status != "success" {
			continue
		}
		anyOK = true
		for i, h := range batch {
			if i < len(res.Response) && res.Response[i] {
				cached[h] = true
			}
		}
	}
	return cached, anyOK
}

func pmGetJSON(ctx context.Context, hc *http.Client, endpoint string, q url.Values, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return http.ErrNotSupported
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}
