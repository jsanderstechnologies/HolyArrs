package sources

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFilterTorrentioItemsAgainstPremiumizeGlobalCache(t *testing.T) {
	const cachedHash = "0123456789abcdef0123456789abcdef01234567"
	const uncachedHash = "ffffffffffffffffffffffffffffffffffffffff"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/cache/check" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		items := r.URL.Query()["items[]"]
		resp := make([]bool, len(items))
		for i, h := range items {
			if h == cachedHash {
				resp[i] = true
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "success",
			"response": resp,
		})
	}))
	defer srv.Close()

	items := []Item{
		{Title: "Full.Metal.Jacket.1987.1080p.BluRay", InfoHash: cachedHash},
		{Title: "Full.Metal.Jacket.1987.4K.Remux", InfoHash: uncachedHash},
	}

	cached, ok := checkPMCloudCache(context.Background(), srv.Client(), srv.URL, "key", items)
	if !ok {
		t.Fatal("checkPMCloudCache failed")
	}
	if !cached[cachedHash] || cached[uncachedHash] {
		t.Fatalf("unexpected cache map: %+v", cached)
	}
}
