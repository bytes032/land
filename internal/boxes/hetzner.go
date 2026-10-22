package boxes

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Hetzner names an SSH key by its MD5 fingerprint
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// hetznerAPI is Hetzner Cloud's API; tests point it at a fake.
var hetznerAPI = "https://api.hetzner.cloud/v1"

var httpClient = &http.Client{Timeout: time.Minute}

// hetzner is the part of Hetzner's Cloud API land calls itself, with HCLOUD_TOKEN, the token crabbox uses too.
type hetzner struct{}

type server struct {
	ID      int64             `json:"id"`
	Name    string            `json:"name"`
	Status  string            `json:"status"`
	Created time.Time         `json:"created"`
	Labels  map[string]string `json:"labels"`
}

type image struct {
	ID      int64             `json:"id"`
	Status  string            `json:"status"` // creating, then available
	Created time.Time         `json:"created"`
	Labels  map[string]string `json:"labels"`
}

// apiError is the API's refusal: its status, code and message, never the request's headers.
type apiError struct {
	method, path  string
	status        int
	code, message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("hetzner %s %s: %d %s: %s", e.method, e.path, e.status, e.code, e.message)
}

func notFound(err error) bool {
	var api *apiError
	return errors.As(err, &api) && api.status == http.StatusNotFound
}

func (h hetzner) do(ctx context.Context, method, path string, body, out any) error {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return errors.New("HCLOUD_TOKEN, the Hetzner project's API token, isn't set")
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, hetznerAPI+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("hetzner %s %s: %w", method, path, err)
	}
	defer func() { _ = response.Body.Close() }() // read-only: Close cannot lose data
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("hetzner %s %s: %w", method, path, err)
	}
	if response.StatusCode >= http.StatusMultipleChoices {
		var refusal struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(data, &refusal) // a body that isn't Hetzner's error shape leaves the status alone
		return &apiError{method, path, response.StatusCode, refusal.Error.Code, refusal.Error.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("hetzner %s %s: %w", method, path, err)
	}
	return nil
}

// list GETs every page of a listing, path with its query, and returns the items under key.
func list[T any](ctx context.Context, h hetzner, path, key string) ([]T, error) {
	var all []T
	for page := 1; page > 0; {
		var reply map[string]json.RawMessage
		if err := h.do(ctx, http.MethodGet, fmt.Sprintf("%s&per_page=50&page=%d", path, page), nil, &reply); err != nil {
			return nil, err
		}
		var items []T
		var meta struct {
			Pagination struct {
				NextPage int `json:"next_page"`
			} `json:"pagination"`
		}
		if err := json.Unmarshal(reply[key], &items); err != nil {
			return nil, fmt.Errorf("hetzner GET %s: %s: %w", path, key, err)
		}
		_ = json.Unmarshal(reply["meta"], &meta) // no meta: the last page
		all = append(all, items...)
		page = meta.Pagination.NextPage
	}
	return all, nil
}

// servers are the project's crabbox servers, those labelled crabbox=true, from every page of the listing.
func (h hetzner) servers(ctx context.Context) ([]server, error) {
	return list[server](ctx, h, "/servers?label_selector="+url.QueryEscape("crabbox=true"), "servers")
}

// slugged are the IDs of the project's crabbox servers labelled slug. The labels are checked again here, so a listing
// that ignored the selector can't hand another server to a deletion.
func (h hetzner) slugged(ctx context.Context, slug string) ([]int64, error) {
	servers, err := list[server](ctx, h, "/servers?label_selector="+url.QueryEscape("crabbox=true,slug="+slug), "servers")
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, s := range servers {
		if crabboxOwned(s.Labels) && s.Labels["slug"] == slug {
			ids = append(ids, s.ID)
		}
	}
	return ids, nil
}

func (h hetzner) server(ctx context.Context, id int64) (server, error) {
	var reply struct {
		Server server `json:"server"`
	}
	err := h.do(ctx, http.MethodGet, fmt.Sprintf("/servers/%d", id), nil, &reply)
	return reply.Server, err
}

// label sets the server's label key to value, or removes it when value is "", keeping its other labels.
func (h hetzner) label(ctx context.Context, id int64, key, value string) error {
	s, err := h.server(ctx, id)
	if err != nil {
		return err
	}
	labels := s.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	if value == "" {
		delete(labels, key)
	} else {
		labels[key] = value
	}
	return h.do(ctx, http.MethodPut, fmt.Sprintf("/servers/%d", id), map[string]any{"labels": labels}, nil)
}

// leaseKey matches the SSH key crabbox registers for a lease, crabbox-cbx-<12 hex>.
var leaseKey = regexp.MustCompile(`^crabbox-cbx-[0-9a-f]{12}$`)

// keys are the IDs of the SSH key crabbox registered for the server's lease, when its labels name that lease's key.
func (h hetzner) keys(ctx context.Context, s server) ([]int64, error) {
	name := s.Labels["provider_key"]
	if !leaseKey.MatchString(name) || name != "crabbox-"+strings.ReplaceAll(s.Labels["lease"], "_", "-") {
		return nil, nil
	}
	var reply struct {
		Keys []struct {
			ID int64 `json:"id"`
		} `json:"ssh_keys"`
	}
	if err := h.do(ctx, http.MethodGet, "/ssh_keys?name="+url.QueryEscape(name), nil, &reply); err != nil {
		return nil, err
	}
	var ids []int64
	for _, k := range reply.Keys {
		ids = append(ids, k.ID)
	}
	return ids, nil
}

// remove deletes a crabbox box: its server, then the SSH key crabbox registered for its lease, as crabbox stop does.
func (h hetzner) remove(ctx context.Context, s server) error {
	if err := h.do(ctx, http.MethodDelete, fmt.Sprintf("/servers/%d", s.ID), nil, nil); err != nil && !notFound(err) {
		return err
	}
	ids, err := h.keys(ctx, s)
	for _, id := range ids {
		if e := h.do(ctx, http.MethodDelete, fmt.Sprintf("/ssh_keys/%d", id), nil, nil); e != nil && !notFound(e) {
			err = errors.Join(err, e)
		}
	}
	return err
}

type sshKey struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// deleteKeys deletes the SSH keys crabbox registered with Hetzner for the public keys under dir, each found by its
// fingerprint, and only one named as crabbox names a lease's key: another key with the same public key stays. A
// checkout there holds the repository's files, not the box's.
func (h hetzner) deleteKeys(ctx context.Context, dir string) error {
	var fingerprints []string
	if err := filepath.WalkDir(dir, func(file string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return err
		case d.IsDir() && file == filepath.Join(dir, "checkout"):
			return fs.SkipDir
		case d.Type().IsRegular() && strings.HasSuffix(file, ".pub"):
			key, err := os.ReadFile(file)
			if f := fingerprint(key); f != "" {
				fingerprints = append(fingerprints, f)
			}
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	for _, f := range fingerprints {
		keys, err := list[sshKey](ctx, h, "/ssh_keys?fingerprint="+url.QueryEscape(f), "ssh_keys")
		if err != nil {
			return err
		}
		for _, k := range keys {
			if !leaseKey.MatchString(k.Name) {
				continue
			}
			if err := h.do(ctx, http.MethodDelete, fmt.Sprintf("/ssh_keys/%d", k.ID), nil, nil); err != nil && !notFound(err) {
				return err
			}
		}
	}
	return nil
}

// fingerprint is the MD5 fingerprint of key, an OpenSSH public key, as Hetzner gives an SSH key's, or "" for no key.
func fingerprint(key []byte) string {
	fields := strings.Fields(string(key))
	if len(fields) < 2 {
		return ""
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return ""
	}
	sum := md5.Sum(blob) //nolint:gosec // as Hetzner's
	pairs := make([]string, len(sum))
	for i, b := range sum {
		pairs[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(pairs, ":")
}

// snapshots are the repository's snapshots, those labelled land=<repo>, newest first. The label is checked again here,
// so a listing that ignored the selector can't hand another repository's image to a box or a deletion.
func (h hetzner) snapshots(ctx context.Context, repo string) ([]image, error) {
	all, err := list[image](ctx, h, "/images?type=snapshot&label_selector="+url.QueryEscape(snapshotLabel+"="+repo), "images")
	if err != nil {
		return nil, err
	}
	all = slices.DeleteFunc(all, func(i image) bool { return i.Labels[snapshotLabel] != repo })
	slices.SortFunc(all, func(a, b image) int { return b.Created.Compare(a.Created) })
	return all, nil
}

func (h hetzner) image(ctx context.Context, id int64) (image, error) {
	var reply struct {
		Image image `json:"image"`
	}
	err := h.do(ctx, http.MethodGet, fmt.Sprintf("/images/%d", id), nil, &reply)
	return reply.Image, err
}
