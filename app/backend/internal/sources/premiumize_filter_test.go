package sources

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFilterTorrentioItemsAgainstPremiumizeCloud(t *testing.T) {
	const availableHash = "0123456789abcdef0123456789abcdef01234567"
	const missingHash = "ffffffffffffffffffffffffffffffffffffffff"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/transfer/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"transfers": []map[string]any{
					{
						"name":   "Full.Metal.Jacket.1987.1080p.BluRay",
						"status": "finished",
						"src":    "magnet:?xt=urn:btih:" + availableHash,
					},
				},
			})
		case "/api/folder/search":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"content": []map[string]any{
					{"name": "Full Metal Jacket (1987)", "type": "folder"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	snap, ok := fetchPMCloudSnapshot(context.Background(), srv.Client(), srv.URL, "key", "Full Metal Jacket")
	if !ok {
		t.Fatal("fetchPMCloudSnapshot failed")
	}

	items := []Item{
		{Title: "Full.Metal.Jacket.1987.1080p.BluRay", InfoHash: availableHash},
		{Title: "Full.Metal.Jacket.1987.4K.Remux", InfoHash: missingHash},
		{Title: "Some.Other.Movie.2024.1080p", InfoHash: missingHash},
	}

	var kept []Item
	for _, it := range items {
		if pmItemAvailable(it, snap) {
			kept = append(kept, it)
		}
	}
	if len(kept) != 2 || kept[0].InfoHash != availableHash {
		t.Fatalf("kept = %+v, expected both Full Metal Jacket items (hash + folder match) and not Some.Other.Movie", kept)
	}
}
