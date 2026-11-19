package downloads

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	domain "github.com/HolyArrs/holyarrs/app/backend/internal/core/model"
)

const (
	pmDefaultBase = "https://www.premiumize.me"
	pmMaxAnswer   = 16 << 20
)

var pmVideoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m4v": true,
	".ts": true, ".m2ts": true, ".wmv": true, ".mov": true,
	".webm": true, ".mpg": true, ".mpeg": true,
}

// Premiumize is a qBittorrent-style torrent Engine backed by the Premiumize.me
// cloud API. Instead of downloading torrent data locally, it finds the matching
// torrent or file already in the user's Premiumize.me cloud and creates .strm
// files containing the Premiumize CDN URLs.
type Premiumize struct {
	base     string
	apiKey   string
	http     *http.Client
	SaveRoot string
	PathMap  []domain.PathMap
	Keep     func(context.Context) ([]string, error)
	Category string
	Others   []string

	mu      sync.Mutex
	jobs    map[string]Job
	pending map[string]pmPending
}

type pmPending struct {
	transferID string
	name       string
	category   string
	saveDir    string
}

type pmAccountInfo struct {
	Status       string `json:"status"`
	Message      string `json:"message"`
	CustomerID   any    `json:"customer_id"`
	PremiumUntil int64  `json:"premium_until"`
}

type pmTransfer struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Message  string  `json:"message"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	Src      string  `json:"src"`
	FolderID string  `json:"folder_id"`
	FileID   string  `json:"file_id"`
}

type pmTransferList struct {
	Status    string       `json:"status"`
	Message   string       `json:"message"`
	Transfers []pmTransfer `json:"transfers"`
}

type pmItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	Link       string `json:"link"`
	StreamLink string `json:"stream_link"`
	Path       string `json:"path"`
	FolderID   string `json:"folder_id"`
	Status     string `json:"status"`
	Message    string `json:"message"`
}

type pmFolderList struct {
	Status  string   `json:"status"`
	Message string   `json:"message"`
	Name    string   `json:"name"`
	Content []pmItem `json:"content"`
}

type pmItemListAll struct {
	Status  string   `json:"status"`
	Message string   `json:"message"`
	Files   []pmItem `json:"files"`
}

type pmCloudFile struct {
	RelPath    string
	Size       int64
	Link       string
	StreamLink string
}

// NewPremiumize returns an Engine that matches torrents in the Premiumize.me
// cloud and writes .strm files with the Premiumize CDN URLs.
func NewPremiumize(base, apiKey string, h *http.Client) *Premiumize {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if b == "" {
		b = pmDefaultBase
	}
	if h == nil {
		h = &http.Client{Timeout: clientTimeout}
	}
	return &Premiumize{
		base:    b,
		apiKey:  strings.TrimSpace(apiKey),
		http:    h,
		jobs:    map[string]Job{},
		pending: map[string]pmPending{},
	}
}

// Health verifies the Premiumize.me API key via /api/account/info.
func (p *Premiumize) Health(ctx context.Context) (Health, error) {
	var info pmAccountInfo
	if err := p.get(ctx, "/api/account/info", nil, &info); err != nil {
		return Health{}, err
	}
	if info.Status != "" && info.Status != "success" {
		return Health{}, pmStatusErr(info.Message)
	}
	return Health{
		Version: "Premiumize.me",
		At:      time.Now(),
		Note:    "Matches torrents already in your Premiumize.me cloud and creates .strm files with CDN links.",
	}, nil
}

// Add finds the matching torrent already in the Premiumize.me cloud, fetches its
// CDN link(s), and creates .strm file(s) in the local save directory.
func (p *Premiumize) Add(ctx context.Context, r AddRequest) (string, error) {
	if p.apiKey == "" {
		return "", ErrAuth
	}
	hash := strings.ToLower(strings.TrimSpace(r.InfoHash))
	if hash == "" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.URL)), "magnet:") {
		hash = linkHash(r.URL)
	}
	if hash == "" && len(r.File) > 0 {
		if h, err := torrentFileHash(r.File); err == nil {
			hash = h
		}
	}
	name := strings.TrimSpace(r.Name)
	if name == "" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.URL)), "magnet:") {
		name = magnetDisplayName(r.URL)
	}
	id := hash
	if id == "" {
		sum := sha1.Sum([]byte(strings.ToLower(name)))
		id = hex.EncodeToString(sum[:])
	}

	saveDir := p.resolveSaveDir(r.SavePath, r.Category)
	if saveDir == "" {
		return "", errors.New("premiumize: no local save folder is configured for .strm files")
	}

	// 1. Check existing transfers in Premiumize.me cloud.
	transfers, err := p.listTransfers(ctx)
	if err != nil {
		return "", err
	}
	if tr, ok := matchPMTransfer(transfers, hash, name); ok {
		st := strings.ToLower(tr.Status)
		switch st {
		case "error", "deleted", "timeout", "banned":
			msg := strings.TrimSpace(tr.Message)
			if msg == "" {
				msg = "transfer status is " + tr.Status
			}
			return "", fmt.Errorf("premiumize transfer failed: %s", msg)
		case "finished":
			files, err := p.filesFromTransfer(ctx, tr)
			if err != nil {
				return "", err
			}
			if _, err := p.materializeStrm(id, firstNonEmpty(name, tr.Name), r.Category, saveDir, files); err != nil {
				return "", err
			}
			return id, nil
		default:
			if tr.Progress >= 1 {
				files, err := p.filesFromTransfer(ctx, tr)
				if err != nil {
					return "", err
				}
				if _, err := p.materializeStrm(id, firstNonEmpty(name, tr.Name), r.Category, saveDir, files); err != nil {
					return "", err
				}
				return id, nil
			}
			// Transfer exists in cloud and is still finishing; track it until finished.
			now := time.Now().Unix()
			p.mu.Lock()
			p.pending[id] = pmPending{transferID: tr.ID, name: firstNonEmpty(name, tr.Name), category: r.Category, saveDir: saveDir}
			p.jobs[id] = Job{
				ID: id, Name: firstNonEmpty(name, tr.Name), Category: r.Category,
				SavePath: saveDir, Progress: tr.Progress, State: StateDownloading,
				Detail: tr.Status, AddedAt: now,
			}
			p.mu.Unlock()
			return id, nil
		}
	}

	// 2. Fallback: search Premiumize.me cloud folders/files directly (in case transfer history was cleared).
	files, matchedName, err := p.findCloudFilesByName(ctx, name)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no matching torrent found in Premiumize.me cloud for %q", name)
	}
	if _, err := p.materializeStrm(id, firstNonEmpty(name, matchedName), r.Category, saveDir, files); err != nil {
		return "", err
	}
	return id, nil
}

// List returns tracked jobs matching category (or all if category is empty).
func (p *Premiumize) List(ctx context.Context, category string) ([]Job, error) {
	p.refreshPending(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Job, 0, len(p.jobs))
	for _, j := range p.jobs {
		if category == "" || sameCategory(j.Category, category) {
			out = append(out, j)
		}
	}
	return out, nil
}

// Get returns one tracked job by id.
func (p *Premiumize) Get(ctx context.Context, id string) (Job, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	p.refreshPending(ctx)
	p.mu.Lock()
	j, ok := p.jobs[id]
	p.mu.Unlock()
	if !ok {
		return Job{}, ErrNotFound
	}
	return j, nil
}

// Remove drops the job from tracking and, if deleteFiles is true, removes the
// local .strm staging directory (never touching library folders or Premiumize cloud).
func (p *Premiumize) Remove(ctx context.Context, id string, deleteFiles bool) error {
	id = strings.ToLower(strings.TrimSpace(id))
	p.mu.Lock()
	j, ok := p.jobs[id]
	delete(p.jobs, id)
	delete(p.pending, id)
	p.mu.Unlock()
	if !ok {
		return nil
	}
	if !deleteFiles || j.SavePath == "" {
		return nil
	}
	local, pathOK := domain.MapPath(j.SavePath, p.PathMap)
	if !pathOK {
		return nil
	}
	local = filepath.Clean(local)
	if p.SaveRoot != "" {
		root := filepath.Clean(p.SaveRoot)
		if local == root || !underAny(local, []string{root}) {
			return nil
		}
	}
	if p.Keep != nil {
		keep, err := p.Keep(ctx)
		if err != nil {
			return err
		}
		for _, k := range keep {
			k = strings.TrimSpace(k)
			if k != "" && (underAny(local, []string{k}) || underAny(filepath.Clean(k), []string{local})) {
				return nil
			}
		}
	}
	return os.RemoveAll(local)
}

func (p *Premiumize) refreshPending(ctx context.Context) {
	p.mu.Lock()
	if len(p.pending) == 0 {
		p.mu.Unlock()
		return
	}
	snap := make(map[string]pmPending, len(p.pending))
	for k, v := range p.pending {
		snap[k] = v
	}
	p.mu.Unlock()

	transfers, err := p.listTransfers(ctx)
	if err != nil {
		return
	}
	byID := make(map[string]pmTransfer, len(transfers))
	for _, tr := range transfers {
		byID[tr.ID] = tr
	}
	for id, pend := range snap {
		tr, ok := byID[pend.transferID]
		if !ok {
			p.mu.Lock()
			if j, exists := p.jobs[id]; exists {
				j.State = StateFailed
				j.Detail = "transfer removed from Premiumize.me"
				p.jobs[id] = j
			}
			delete(p.pending, id)
			p.mu.Unlock()
			continue
		}
		st := strings.ToLower(tr.Status)
		if st == "finished" || tr.Progress >= 1 {
			files, err := p.filesFromTransfer(ctx, tr)
			if err != nil || len(files) == 0 {
				p.mu.Lock()
				if j, exists := p.jobs[id]; exists {
					j.State = StateFailed
					if err != nil {
						j.Detail = err.Error()
					} else {
						j.Detail = "no video files found in Premiumize.me transfer"
					}
					p.jobs[id] = j
				}
				delete(p.pending, id)
				p.mu.Unlock()
				continue
			}
			if _, err := p.materializeStrm(id, pend.name, pend.category, pend.saveDir, files); err == nil {
				p.mu.Lock()
				delete(p.pending, id)
				p.mu.Unlock()
			}
		} else if st == "error" || st == "deleted" || st == "timeout" || st == "banned" {
			p.mu.Lock()
			if j, exists := p.jobs[id]; exists {
				j.State = StateFailed
				j.Detail = firstNonEmpty(tr.Message, tr.Status)
				p.jobs[id] = j
			}
			delete(p.pending, id)
			p.mu.Unlock()
		} else {
			p.mu.Lock()
			if j, exists := p.jobs[id]; exists {
				j.Progress = tr.Progress
				j.Detail = tr.Status
				p.jobs[id] = j
			}
			p.mu.Unlock()
		}
	}
}

func (p *Premiumize) materializeStrm(id, name, category, saveDir string, files []pmCloudFile) (Job, error) {
	videos := filterVideoCloudFiles(files)
	if len(videos) == 0 {
		return Job{}, fmt.Errorf("no video files with CDN links found in Premiumize.me for %q", name)
	}
	releaseDir := filepath.Join(saveDir, pmSafeName(name))
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		return Job{}, fmt.Errorf("premiumize: create folder %q: %w", releaseDir, err)
	}

	var jobFiles []File
	var totalSize int64
	for _, vf := range videos {
		cdn := strings.TrimSpace(vf.Link)
		if cdn == "" {
			cdn = strings.TrimSpace(vf.StreamLink)
		}
		if cdn == "" {
			continue
		}
		rel := strings.TrimSuffix(vf.RelPath, filepath.Ext(vf.RelPath)) + ".strm"
		fullPath := filepath.Join(releaseDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return Job{}, fmt.Errorf("premiumize: create parent folder: %w", err)
		}
		content := cdn + "\n"
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			return Job{}, fmt.Errorf("premiumize: write strm %q: %w", fullPath, err)
		}
		sz := int64(len(content))
		totalSize += sz
		jobFiles = append(jobFiles, File{
			Name:     fullPath,
			Size:     sz,
			Progress: 1,
			Priority: 1,
		})
	}
	if len(jobFiles) == 0 {
		return Job{}, fmt.Errorf("no playable CDN links returned by Premiumize.me for %q", name)
	}

	now := time.Now().Unix()
	j := Job{
		ID:          id,
		Name:        name,
		Category:    category,
		SavePath:    releaseDir,
		ContentPath: releaseDir,
		Size:        totalSize,
		Progress:    1,
		State:       StateFinished,
		Detail:      "strm_ready",
		Files:       jobFiles,
		AddedAt:     now,
		DoneAt:      now,
	}
	p.mu.Lock()
	p.jobs[id] = j
	p.mu.Unlock()
	return j, nil
}

func (p *Premiumize) resolveSaveDir(reqSave, category string) string {
	base := strings.TrimSpace(reqSave)
	if base == "" {
		base = strings.TrimSpace(p.SaveRoot)
	}
	if base == "" {
		return ""
	}
	base, ok := domain.MapPath(base, p.PathMap)
	if !ok {
		return ""
	}
	cat := strings.TrimSpace(category)
	if cat != "" && !strings.EqualFold(filepath.Base(base), cat) {
		return filepath.Join(base, pmSafeName(cat))
	}
	return base
}

func (p *Premiumize) listTransfers(ctx context.Context) ([]pmTransfer, error) {
	var out pmTransferList
	if err := p.get(ctx, "/api/transfer/list", nil, &out); err != nil {
		return nil, err
	}
	if out.Status != "" && out.Status != "success" {
		return nil, pmStatusErr(out.Message)
	}
	return out.Transfers, nil
}

func (p *Premiumize) filesFromTransfer(ctx context.Context, tr pmTransfer) ([]pmCloudFile, error) {
	if tr.FolderID != "" {
		return p.collectFolderFiles(ctx, tr.FolderID, "")
	}
	if tr.FileID != "" {
		it, err := p.itemDetails(ctx, tr.FileID)
		if err != nil {
			return nil, err
		}
		return []pmCloudFile{{RelPath: it.Name, Size: it.Size, Link: it.Link, StreamLink: it.StreamLink}}, nil
	}
	return nil, errors.New("premiumize transfer has neither folder_id nor file_id")
}

func (p *Premiumize) collectFolderFiles(ctx context.Context, folderID, prefix string) ([]pmCloudFile, error) {
	q := url.Values{}
	q.Set("id", folderID)
	q.Set("includebreadcrumbs", "false")
	var list pmFolderList
	if err := p.get(ctx, "/api/folder/list", q, &list); err != nil {
		return nil, err
	}
	if list.Status != "" && list.Status != "success" {
		return nil, pmStatusErr(list.Message)
	}
	var out []pmCloudFile
	for _, it := range list.Content {
		rel := it.Name
		if prefix != "" {
			rel = prefix + "/" + it.Name
		}
		if it.Type == "folder" {
			sub, err := p.collectFolderFiles(ctx, it.ID, rel)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
			continue
		}
		out = append(out, pmCloudFile{
			RelPath:    rel,
			Size:       it.Size,
			Link:       it.Link,
			StreamLink: it.StreamLink,
		})
	}
	return out, nil
}

func (p *Premiumize) itemDetails(ctx context.Context, id string) (pmItem, error) {
	q := url.Values{}
	q.Set("id", id)
	var it pmItem
	if err := p.get(ctx, "/api/item/details", q, &it); err != nil {
		return pmItem{}, err
	}
	if it.Status != "" && it.Status != "success" {
		return pmItem{}, pmStatusErr(it.Message)
	}
	return it, nil
}

func (p *Premiumize) findCloudFilesByName(ctx context.Context, name string) ([]pmCloudFile, string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, "", nil
	}
	q := url.Values{}
	q.Set("q", pmSearchTerm(name))
	var res pmFolderList
	if err := p.get(ctx, "/api/folder/search", q, &res); err == nil && res.Status == "success" {
		for _, it := range res.Content {
			if it.Type == "folder" && pmSameName(it.Name, name) {
				files, err := p.collectFolderFiles(ctx, it.ID, "")
				return files, it.Name, err
			}
			if it.Type == "file" && pmVideoExts[strings.ToLower(filepath.Ext(it.Name))] && pmSameName(strings.TrimSuffix(it.Name, filepath.Ext(it.Name)), name) {
				det, err := p.itemDetails(ctx, it.ID)
				if err != nil {
					return nil, "", err
				}
				return []pmCloudFile{{RelPath: det.Name, Size: det.Size, Link: det.Link, StreamLink: det.StreamLink}}, det.Name, nil
			}
		}
	}

	// Fallback to /api/item/listall for direct file matches.
	var all pmItemListAll
	if err := p.get(ctx, "/api/item/listall", nil, &all); err != nil {
		return nil, "", err
	}
	if all.Status != "" && all.Status != "success" {
		return nil, "", pmStatusErr(all.Message)
	}
	for _, it := range all.Files {
		if !pmVideoExts[strings.ToLower(filepath.Ext(it.Name))] {
			continue
		}
		base := strings.TrimSuffix(it.Name, filepath.Ext(it.Name))
		if pmSameName(base, name) || pmPathFolderMatches(it.Path, name) {
			det, err := p.itemDetails(ctx, it.ID)
			if err != nil {
				return nil, "", err
			}
			return []pmCloudFile{{RelPath: det.Name, Size: det.Size, Link: det.Link, StreamLink: det.StreamLink}}, det.Name, nil
		}
	}
	return nil, "", nil
}

func (p *Premiumize) get(ctx context.Context, endpoint string, q url.Values, dst any) error {
	if q == nil {
		q = url.Values{}
	}
	q.Set("apikey", p.apiKey)
	u := p.base + endpoint + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrAuth
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("premiumize %s: %s", endpoint, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, pmMaxAnswer+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > pmMaxAnswer {
		return ErrTooLarge
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("premiumize %s: %w", endpoint, err)
	}
	return nil
}

func matchPMTransfer(transfers []pmTransfer, hash, name string) (pmTransfer, bool) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if hash != "" {
		for _, tr := range transfers {
			if srcHash := linkHash(tr.Src); srcHash != "" && srcHash == hash {
				return tr, true
			}
			if strings.Contains(strings.ToLower(tr.Src), hash) {
				return tr, true
			}
		}
	}
	if name != "" {
		for _, tr := range transfers {
			if pmSameName(tr.Name, name) {
				return tr, true
			}
		}
	}
	return pmTransfer{}, false
}

func filterVideoCloudFiles(files []pmCloudFile) []pmCloudFile {
	var videos []pmCloudFile
	for _, f := range files {
		if !pmVideoExts[strings.ToLower(filepath.Ext(f.RelPath))] {
			continue
		}
		if strings.TrimSpace(f.Link) == "" && strings.TrimSpace(f.StreamLink) == "" {
			continue
		}
		videos = append(videos, f)
	}
	if len(videos) <= 1 {
		return videos
	}
	var nonSample []pmCloudFile
	for _, v := range videos {
		low := strings.ToLower(filepath.Base(v.RelPath))
		if strings.Contains(low, "sample") {
			continue
		}
		nonSample = append(nonSample, v)
	}
	if len(nonSample) > 0 {
		return nonSample
	}
	return videos
}

func pmStatusErr(msg string) error {
	m := strings.TrimSpace(msg)
	low := strings.ToLower(m)
	if strings.Contains(low, "not logged") || strings.Contains(low, "api key") || strings.Contains(low, "apikey") || strings.Contains(low, "auth") {
		return fmt.Errorf("%w: %s", ErrAuth, m)
	}
	if m == "" {
		m = "premiumize returned an error"
	}
	return errors.New(m)
}

func pmNormalize(s string) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastSpace = false
		default:
			if !lastSpace && b.Len() > 0 {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func pmSameName(a, b string) bool {
	na, nb := pmNormalize(a), pmNormalize(b)
	return na != "" && na == nb
}

func pmPathFolderMatches(p, name string) bool {
	for _, part := range strings.Split(p, "/") {
		if pmSameName(part, name) {
			return true
		}
	}
	return false
}

func pmSearchTerm(name string) string {
	n := pmNormalize(name)
	parts := strings.Fields(n)
	if len(parts) > 4 {
		parts = parts[:4]
	}
	return strings.Join(parts, " ")
}

func pmSafeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		default:
			if r < 0x20 {
				return -1
			}
			return r
		}
	}, s)
	s = strings.Trim(s, ". ")
	if s == "" {
		return "release"
	}
	return s
}

func magnetDisplayName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Query().Get("dn"))
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return strings.TrimSpace(b)
}
