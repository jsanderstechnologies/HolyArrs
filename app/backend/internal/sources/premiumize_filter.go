package sources

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/HolyArrs/holyarrs/app/backend/internal/core/database"
	domain "github.com/HolyArrs/holyarrs/app/backend/internal/core/model"
	"github.com/HolyArrs/holyarrs/app/backend/internal/core/outbound"
	"github.com/HolyArrs/holyarrs/app/backend/internal/core/releasename"
)

var pmBtihRe = regexp.MustCompile(`(?i)btih:([0-9a-f]{40})`)

type pmCloudSnapshot struct {
	hashes map[string]bool
	names  []string
}

// filterTorrentioByPremiumize filters Torrentio results so only items already
// available in the user's configured Premiumize.me cloud are returned.
// If no Premiumize.me downloader is configured, items are returned unchanged.
func filterTorrentioByPremiumize(ctx context.Context, store *database.Store, secrets SecretReader, q Query, items []Item) []Item {
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
	snap, ok := fetchPMCloudSnapshot(ctx, client, base, strings.TrimSpace(apiKey), q.Title)
	if !ok {
		return items
	}
	out := make([]Item, 0, len(items))
	for _, it := range items {
		if pmItemAvailable(it, snap) {
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
		Transport: outbound.NewTransport(allow),
	}
}

func fetchPMCloudSnapshot(ctx context.Context, hc *http.Client, base, apiKey, title string) (pmCloudSnapshot, bool) {
	snap := pmCloudSnapshot{hashes: map[string]bool{}}

	var trList struct {
		Status    string `json:"status"`
		Transfers []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Src    string `json:"src"`
		} `json:"transfers"`
	}
	if err := pmGetJSON(ctx, hc, base+"/api/transfer/list", url.Values{"apikey": {apiKey}}, &trList); err != nil {
		return snap, false
	}
	for _, tr := range trList.Transfers {
		st := strings.ToLower(tr.Status)
		if st == "error" || st == "deleted" || st == "timeout" || st == "banned" {
			continue
		}
		if m := pmBtihRe.FindStringSubmatch(tr.Src); len(m) == 2 {
			snap.hashes[strings.ToLower(m[1])] = true
		}
		if strings.TrimSpace(tr.Name) != "" {
			snap.names = append(snap.names, tr.Name)
		}
	}

	if strings.TrimSpace(title) != "" {
		var searchRes struct {
			Status  string `json:"status"`
			Content []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"content"`
		}
		if err := pmGetJSON(ctx, hc, base+"/api/folder/search", url.Values{"apikey": {apiKey}, "q": {title}}, &searchRes); err == nil && searchRes.Status == "success" {
			for _, c := range searchRes.Content {
				if c.Type == "folder" {
					snap.names = append(snap.names, c.Name)
				} else if c.Type == "file" {
					snap.names = append(snap.names, strings.TrimSuffix(c.Name, filepath.Ext(c.Name)))
				}
			}
		}
	}

	if len(snap.names) == 0 {
		var all struct {
			Status string `json:"status"`
			Files  []struct {
				Name string `json:"name"`
				Path string `json:"path"`
			} `json:"files"`
		}
		if err := pmGetJSON(ctx, hc, base+"/api/item/listall", url.Values{"apikey": {apiKey}}, &all); err == nil && all.Status == "success" {
			for _, f := range all.Files {
				snap.names = append(snap.names, strings.TrimSuffix(f.Name, filepath.Ext(f.Name)))
				for _, part := range strings.Split(f.Path, "/") {
					if strings.TrimSpace(part) != "" {
						snap.names = append(snap.names, part)
					}
				}
			}
		}
	}

	return snap, true
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

func pmItemAvailable(it Item, snap pmCloudSnapshot) bool {
	if h := strings.ToLower(strings.TrimSpace(it.InfoHash)); h != "" && snap.hashes[h] {
		return true
	}
	for _, n := range snap.names {
		if pmCloudNameMatches(n, it.Title) {
			return true
		}
	}
	return false
}

func pmCloudNameMatches(cand, want string) bool {
	nc, nw := pmNorm(cand), pmNorm(want)
	if nc == "" || nw == "" {
		return false
	}
	if nc == nw {
		return true
	}
	cp := releasename.Parse(cand, releasename.Hints{})
	wp := releasename.Parse(want, releasename.Hints{})
	wt := pmNorm(wp.Title.Value)
	if wt == "" {
		return false
	}
	ct := pmNorm(cp.Title.Value)
	if ct != "" && ct == wt {
		if wp.Year.Value > 0 && cp.Year.Value > 0 && cp.Year.Value != wp.Year.Value {
			return false
		}
	} else {
		if nc != wt && !strings.HasPrefix(nc, wt+" ") {
			return false
		}
		if wp.Year.Value > 0 {
			yr := strconv.Itoa(wp.Year.Value)
			if !strings.Contains(nc, yr) && cp.Year.Value > 0 && cp.Year.Value != wp.Year.Value {
				return false
			}
		}
	}
	if wp.Show != nil {
		if cp.Show == nil {
			return false
		}
		if len(wp.Show.Seasons) > 0 && !pmIntOverlap(cp.Show.Seasons, wp.Show.Seasons) {
			return false
		}
		if len(wp.Show.Episodes) > 0 && !pmIntOverlap(cp.Show.Episodes, wp.Show.Episodes) {
			return false
		}
	}
	return true
}

func pmNorm(s string) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastSpace = false
		} else if !lastSpace && b.Len() > 0 {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func pmIntOverlap(a, b []int) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
