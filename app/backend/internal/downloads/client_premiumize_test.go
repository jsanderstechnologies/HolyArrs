package downloads

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPremiumizeHealthAndAddCreatesStrm(t *testing.T) {
	const apiKey = "pm-secret-key"
	const hash = "0123456789abcdef0123456789abcdef01234567"
	const cdnURL = "https://cdn.premiumize.me/dl/abc123/Full.Metal.Jacket.1987.1080p.BluRay.mkv"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") != apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/account/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "customer_id": 12345})
		case "/api/transfer/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"transfers": []map[string]any{
					{
						"id":        "tr-1",
						"name":      "Full.Metal.Jacket.1987.1080p.BluRay.x264-GRP",
						"status":    "finished",
						"progress":  1,
						"src":       "magnet:?xt=urn:btih:" + hash,
						"folder_id": "fld-1",
					},
				},
			})
		case "/api/folder/list":
			if r.URL.Query().Get("id") != "fld-1" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"name":   "Full.Metal.Jacket.1987.1080p.BluRay.x264-GRP",
				"content": []map[string]any{
					{"id": "f-sample", "name": "sample.mkv", "type": "file", "size": 500, "link": "https://cdn.premiumize.me/dl/sample.mkv"},
					{"id": "f-main", "name": "Full.Metal.Jacket.1987.1080p.BluRay.mkv", "type": "file", "size": 8000000000, "link": cdnURL},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	saveRoot := t.TempDir()
	pm := NewPremiumize(srv.URL, apiKey, srv.Client())
	pm.SaveRoot = saveRoot

	ctx := context.Background()
	h, err := pm.Health(ctx)
	if err != nil || h.Version != "Premiumize.me" {
		t.Fatalf("Health() = %+v, %v", h, err)
	}

	id, err := pm.Add(ctx, AddRequest{
		URL:      "magnet:?xt=urn:btih:" + hash,
		Name:     "Full.Metal.Jacket.1987.1080p.BluRay.x264-GRP",
		Category: "movies",
		InfoHash: hash,
	})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if id != hash {
		t.Fatalf("Add() id = %q, want %q", id, hash)
	}

	job, err := pm.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if job.State != StateFinished || len(job.Files) != 1 {
		t.Fatalf("Get() = %+v", job)
	}

	strmPath := filepath.Join(job.SavePath, filepath.FromSlash(job.Files[0].Name))
	if filepath.Ext(strmPath) != ".strm" || filepath.IsAbs(job.Files[0].Name) {
		t.Fatalf("expected relative .strm file, got %q", job.Files[0].Name)
	}
	b, err := os.ReadFile(strmPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", strmPath, err)
	}
	if strings.TrimSpace(string(b)) != cdnURL {
		t.Fatalf(".strm content = %q, want %q", string(b), cdnURL)
	}

	// Remove with deleteFiles removes the staging folder.
	if err := pm.Remove(ctx, id, true); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(strmPath); !os.IsNotExist(err) {
		t.Fatalf("expected %q to be removed", strmPath)
	}
}

func TestPremiumizeAddNotFoundInCloud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/transfer/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "transfers": []any{}})
		case "/api/folder/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "content": []any{}})
		case "/api/item/listall":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "files": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	pm := NewPremiumize(srv.URL, "key", srv.Client())
	pm.SaveRoot = t.TempDir()

	_, err := pm.Add(context.Background(), AddRequest{
		Name:     "Full.Metal.Jacket.1987.1080p.BluRay.x264-GRP",
		Category: "movies",
		InfoHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err == nil || !strings.Contains(err.Error(), "no matching torrent found in Premiumize.me cloud") {
		t.Fatalf("expected not-found error, got %v", err)
	}
	if fe, ok := err.(interface{ Final() bool }); !ok || !fe.Final() {
		t.Fatalf("expected final releaseRefused error so grab moves to next release, got %T", err)
	}
}

func TestPremiumizeMatchesTitleAndYearInCloudFolder(t *testing.T) {
	const cdnURL = "https://cdn.premiumize.me/dl/xyz/Full.Metal.Jacket.1987.mkv"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/transfer/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "transfers": []any{}})
		case "/api/folder/search":
			if strings.EqualFold(r.URL.Query().Get("q"), "full metal jacket") {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "success",
					"content": []map[string]any{
						{"id": "fld-fmj", "name": "Full Metal Jacket (1987)", "type": "folder"},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "content": []any{}})
		case "/api/folder/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"content": []map[string]any{
					{"id": "f-1", "name": "Full.Metal.Jacket.1987.mkv", "type": "file", "size": 5000000000, "link": cdnURL},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	pm := NewPremiumize(srv.URL, "key", srv.Client())
	pm.SaveRoot = t.TempDir()

	id, err := pm.Add(context.Background(), AddRequest{
		Name:     "Full Metal Jacket 1987 REPACK UHD BluRay 1080p DDP 5 1 DoVi HDR10 x265-SM737",
		Category: "movies",
		InfoHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	job, err := pm.Get(context.Background(), id)
	if err != nil || job.State != StateFinished || len(job.Files) != 1 {
		t.Fatalf("Get() = %+v, %v", job, err)
	}
	strmPath := filepath.Join(job.SavePath, filepath.FromSlash(job.Files[0].Name))
	b, err := os.ReadFile(strmPath)
	if err != nil || strings.TrimSpace(string(b)) != cdnURL {
		t.Fatalf(".strm content = %q, %v", string(b), err)
	}
}

func TestPremiumizeAddUsesGlobalCacheDirectDL(t *testing.T) {
	const hash = "1111111111111111111111111111111111111111"
	const cdnURL = "https://cdn.premiumize.me/dl/global/Full.Metal.Jacket.1987.mkv"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/transfer/directdl" && r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"content": []map[string]any{
					{
						"path": "Full.Metal.Jacket.1987/Full.Metal.Jacket.1987.mkv",
						"size": 8000000000,
						"link": cdnURL,
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	pm := NewPremiumize(srv.URL, "key", srv.Client())
	pm.SaveRoot = t.TempDir()

	id, err := pm.Add(context.Background(), AddRequest{
		URL:      "magnet:?xt=urn:btih:" + hash,
		Name:     "Full.Metal.Jacket.1987.1080p.BluRay",
		Category: "movies",
		InfoHash: hash,
	})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	job, err := pm.Get(context.Background(), id)
	if err != nil || job.State != StateFinished || len(job.Files) != 1 {
		t.Fatalf("Get() = %+v, %v", job, err)
	}
	b, err := os.ReadFile(filepath.Join(job.SavePath, filepath.FromSlash(job.Files[0].Name)))
	if err != nil || strings.TrimSpace(string(b)) != cdnURL {
		t.Fatalf(".strm content = %q, %v", string(b), err)
	}
}
