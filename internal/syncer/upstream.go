package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

type File struct {
	Name        string `json:"name"`
	SHA         string `json:"sha"`
	DownloadURL string `json:"download_url"`
	Type        string `json:"type"`
}
type Client struct {
	Repo, Branch, Token string
	HTTP                *http.Client
}

func New(repo, branch, token string) *Client {
	retry := retryablehttp.NewClient()
	retry.RetryMax = 4
	retry.RetryWaitMin = 500 * time.Millisecond
	retry.RetryWaitMax = 5 * time.Second
	retry.Logger = nil
	httpClient := retry.StandardClient()
	httpClient.Timeout = 30 * time.Second
	return &Client{Repo: repo, Branch: branch, Token: token, HTTP: httpClient}
}

func (c *Client) FetchRuleYAML(ctx context.Context) (map[string][]byte, map[string]string, error) {
	parts := strings.SplitN(c.Repo, "/", 2)
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("UPSTREAM_REPO must be owner/repository")
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/rule?ref=%s", parts[0], parts[1], c.Branch)
	var files []File
	if err := c.get(ctx, endpoint, &files); err != nil {
		return nil, nil, err
	}
	out := map[string][]byte{}
	shas := map[string]string{}
	for _, f := range files {
		if f.Type != "file" || !strings.HasSuffix(strings.ToLower(f.Name), ".yaml") {
			continue
		}
		if !(strings.HasPrefix(f.Name, "Custom_Direct_") || strings.HasPrefix(f.Name, "Custom_Proxy_") || f.Name == "Custom_Port_Direct.yaml") {
			continue
		}
		b, err := c.bytes(ctx, f.DownloadURL)
		if err != nil {
			return nil, nil, fmt.Errorf("download upstream %s: %w", f.Name, err)
		}
		out[f.Name] = b
		shas[f.Name] = f.SHA
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("no matching upstream YAML rules found")
	}
	return out, shas, nil
}

func (c *Client) get(ctx context.Context, endpoint string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("User-Agent", "ClashRulePilot/1.0")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(result)
}
func (c *Client) bytes(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ClashRulePilot/1.0")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 20<<20))
}
