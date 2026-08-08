package gitlab

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	gl "gitlab.com/gitlab-org/api/client-go"
)

type Client struct {
	webBase, project, branch string
	owner, repo              string
	startBranch              string
	needsBranch              bool
	api                      *gl.Client
}

func New(baseURL, token, project, branch string) (*Client, error) {
	webBase := strings.TrimRight(baseURL, "/")
	apiBase := webBase
	if !strings.HasSuffix(apiBase, "/api/v4") {
		apiBase += "/api/v4"
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
	httpClient := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	api, err := gl.NewClient(token,
		gl.WithHTTPClient(httpClient),
		gl.WithCustomRetry(func(ctx context.Context, resp *http.Response, err error) (bool, error) {
			if err != nil {
				message := strings.ToLower(err.Error())
				if errors.Is(err, io.EOF) || strings.Contains(message, "unexpected eof") || strings.Contains(message, "tls") {
					return true, nil
				}
			}
			return retryablehttp.DefaultRetryPolicy(ctx, resp, err)
		}),
		gl.WithBaseURL(apiBase),
		gl.WithCustomRetryMax(4),
		gl.WithCustomRetryWaitMinMax(500*time.Millisecond, 5*time.Second),
		gl.WithCustomLogger(nil),
		gl.WithUserAgent("ClashRulePilot/1.0"),
	)
	if err != nil {
		return nil, err
	}
	c := &Client{webBase: webBase, project: strings.Trim(project, "/"), branch: branch, api: api}
	parts := strings.Split(c.project, "/")
	c.repo = parts[len(parts)-1]
	if len(parts) > 1 {
		c.owner = strings.Join(parts[:len(parts)-1], "/")
	}
	return c, nil
}

func (c *Client) Owner() string  { return c.owner }
func (c *Client) Repo() string   { return c.repo }
func (c *Client) Branch() string { return c.branch }
func (c *Client) WebURL() string { return c.webBase + "/" + c.project }
func (c *Client) RawURL(file string) string {
	return fmt.Sprintf("%s/%s/-/raw/%s/%s", c.webBase, c.project, c.branch, strings.TrimLeft(file, "/"))
}

func (c *Client) EnsureRepo(ctx context.Context) error {
	project, resp, err := c.api.Projects.GetProject(c.project, nil, gl.WithContext(ctx))
	if err == nil {
		c.applyProject(project)
		c.checkBranch(ctx)
		return nil
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("gitlab project: %w", err)
	}
	opt := &gl.CreateProjectOptions{
		Name: gl.Ptr(c.repo), Path: gl.Ptr(c.repo), Visibility: gl.Ptr(gl.PublicVisibility), InitializeWithReadme: gl.Ptr(true),
	}
	if c.owner != "" {
		if ns, nsResp, nsErr := c.api.Namespaces.GetNamespace(c.owner, gl.WithContext(ctx)); nsErr == nil && nsResp.StatusCode < 300 {
			opt.NamespaceID = gl.Ptr(ns.ID)
		}
	}
	project, _, err = c.api.Projects.CreateProject(opt, gl.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("create GitLab rules repository: %w", err)
	}
	c.applyProject(project)
	c.checkBranch(ctx)
	return nil
}

func (c *Client) applyProject(p *gl.Project) {
	if p == nil {
		return
	}
	if p.PathWithNamespace != "" {
		c.project = p.PathWithNamespace
		parts := strings.Split(c.project, "/")
		c.repo = parts[len(parts)-1]
		c.owner = strings.Join(parts[:len(parts)-1], "/")
	}
	if c.branch == "" {
		c.branch = p.DefaultBranch
	}
	if c.branch == "" {
		c.branch = "main"
	}
	c.startBranch = p.DefaultBranch
	if c.startBranch == "" {
		c.startBranch = "main"
	}
}

func (c *Client) checkBranch(ctx context.Context) {
	_, resp, err := c.api.Branches.GetBranch(c.project, c.branch, gl.WithContext(ctx))
	c.needsBranch = err != nil && resp != nil && resp.StatusCode == http.StatusNotFound && c.startBranch != ""
}

func (c *Client) GetFile(ctx context.Context, file string) ([]byte, string, error) {
	result, resp, err := c.api.RepositoryFiles.GetFile(c.project, strings.TrimLeft(file, "/"), &gl.GetFileOptions{Ref: gl.Ptr(c.branch)}, gl.WithContext(ctx))
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, "", nil
		}
		return nil, "", err
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(result.Content, "\n", ""))
	if err != nil {
		return nil, "", fmt.Errorf("decode %s: %w", file, err)
	}
	return b, result.LastCommitID, nil
}

func (c *Client) CommitFiles(ctx context.Context, files map[string][]byte, message string) (string, error) {
	actions := make([]*gl.CommitActionOptions, 0, len(files))
	for file, content := range files {
		_, lastCommit, err := c.GetFile(ctx, file)
		if err != nil {
			return "", err
		}
		action := gl.FileCreate
		if lastCommit != "" {
			action = gl.FileUpdate
		}
		encoded := base64.StdEncoding.EncodeToString(content)
		item := &gl.CommitActionOptions{Action: &action, FilePath: gl.Ptr(file), Content: &encoded, Encoding: gl.Ptr("base64")}
		if lastCommit != "" {
			item.LastCommitID = &lastCommit
		}
		actions = append(actions, item)
	}
	opt := &gl.CreateCommitOptions{Branch: gl.Ptr(c.branch), CommitMessage: &message, Actions: actions}
	if c.needsBranch {
		opt.StartBranch = &c.startBranch
	}
	commit, _, err := c.api.Commits.CreateCommit(c.project, opt, gl.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("GitLab atomic commit: %w", err)
	}
	c.needsBranch = false
	return commit.ID, nil
}
