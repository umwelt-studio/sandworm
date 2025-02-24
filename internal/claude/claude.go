// Package claude provides functionality for interacting with the Claude API.
package claude

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/holonoms/sandworm/internal/config"
)

const (
	baseURL = "https://api.claude.ai"

	// Configuration keys
	sessionKey     = "claude.session_key" // Global, used across sandworm projects
	organizationID = "claude.organization_id"
	projectID      = "claude.project_id"
	documentID     = "claude.document_id"
	contentHashKey = "claude.content_hash" // Stores hash of last uploaded content
)

var sessionKeyRegex = regexp.MustCompile(`^sessionKey=([^;]+)`)

// Client manages interactions with the Claude API
type Client struct {
	config     *config.Config
	httpClient *http.Client
}

// New creates a new Claude API client using the provided configuration
func New(conf *config.Config) *Client {
	return &Client{
		config: conf,
		httpClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
				},
				Dial: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 5 * time.Second,
				}).Dial,
				TLSHandshakeTimeout: 5 * time.Second,
			},
		},
	}
}

// MARK: Interface

// Setup initializes the client configuration, prompting for required values
// if they're not already set. It validates organization access and project
// selection.
func (c *Client) Setup(force bool) (bool, error) {
	// Handle session key setup
	if force || !c.config.Has(sessionKey) {
		fmt.Println("\nPlease go to https://claude.ai in your browser and copy your session key from the Cookie header.")
		fmt.Println("You can find this in your browser's developer tools under Network tab.")
		fmt.Println()
		fmt.Print("Enter your session key: ")
		var key string
		if _, err := fmt.Scanln(&key); err != nil {
			return false, fmt.Errorf("failed to read session key: %w", err)
		}
		if err := c.config.Set(sessionKey, key); err != nil {
			return false, err
		}
	}

	// Handle organization selection
	if force || !c.config.Has(organizationID) {
		orgs, err := c.listOrganizations()
		if err != nil {
			return false, err
		}

		if len(orgs) == 0 {
			fmt.Println("\nNo organizations found. Please create one at https://claude.ai")
			return false, nil
		}

		fmt.Println("\nSelect an organization for this project:")
		org := selectFromList(orgs)
		if err := c.config.Set(organizationID, org.ID); err != nil {
			return false, err
		}
	}

	// Handle project selection
	if force || !c.config.Has(projectID) {
		projects, err := c.listProjects()
		if err != nil {
			return false, err
		}

		var activeProjects []project
		for _, p := range projects {
			if p.ArchivedAt.IsZero() {
				activeProjects = append(activeProjects, p)
			}
		}

		if len(activeProjects) == 0 {
			fmt.Println("\nNo active projects found. Please create one at https://claude.ai")
			return false, nil
		}

		fmt.Println("\nSelect a project:")
		project := selectFromList(activeProjects)
		if err := c.config.Set(projectID, project.ID); err != nil {
			return false, err
		}
	}

	return true, nil
}

// Push uploads a file to the selected Claude project. If a file with the same
// name exists, it's replaced, but only if the content has changed.
func (c *Client) Push(filePath, fileName string) error {
	if err := c.validateConfig(); err != nil {
		return err
	}

	// Read new file content
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	// Calculate content hash
	contentHash := calculateContentHash(content)

	// Check if content is unchanged from last push
	if c.config.Has(contentHashKey) && c.config.Get(contentHashKey) == contentHash {
		// If we already have a document ID and the content is unchanged,
		// no need to re-upload
		if c.config.Has(documentID) {
			fmt.Println("Content unchanged, skipping upload.")
			return nil
		}
	}

	// If no document ID is set, try to find existing document
	if !c.config.Has(documentID) {
		docs, err := c.listDocuments()
		if err != nil {
			return err
		}
		for _, doc := range docs {
			if doc.FileName == fileName {
				if err := c.config.Set(documentID, doc.ID); err != nil {
					return err
				}
				break
			}
		}
	}

	// Delete existing document if we have one
	if c.config.Has(documentID) {
		if err := c.deleteDocument(c.config.Get(documentID)); err != nil {
			// Only return error if it's not a 404
			if !strings.Contains(err.Error(), "404") {
				return err
			}
		}
		if err := c.config.Delete(documentID); err != nil {
			return err
		}
	}

	// Upload new document
	doc, err := c.uploadDocument(fileName, string(content))
	if err != nil {
		return err
	}

	// Store document ID and content hash
	if err := c.config.Set(documentID, doc.ID); err != nil {
		return err
	}
	return c.config.Set(contentHashKey, contentHash)
}

// PurgeProjectFiles removes all files from the current project.
func (c *Client) PurgeProjectFiles(progressFn func(fileName string, current, total int)) (int, error) {
	if err := c.validateConfig(); err != nil {
		return 0, err
	}

	docs, err := c.listDocuments()
	if err != nil {
		return 0, err
	}

	for i, doc := range docs {
		if progressFn != nil {
			progressFn(doc.FileName, i+1, len(docs))
		}

		if err := c.deleteDocument(doc.ID); err != nil {
			// Only return error if it's not a 404
			if !strings.Contains(err.Error(), "404") {
				return i, err
			}
		}
	}

	// Clear stored document ID and content hash
	if err := c.config.Delete(documentID); err != nil {
		return len(docs), err
	}
	if err := c.config.Delete(contentHashKey); err != nil {
		return len(docs), err
	}

	return len(docs), nil
}

// MARK: Internal helper functions

// calculateContentHash computes a SHA-256 hash of the content
func calculateContentHash(content []byte) string {
	hash := sha256.Sum256(content)
	return hex.EncodeToString(hash[:])
}

// validateConfig ensures all required configuration values are present
func (c *Client) validateConfig() error {
	required := []string{sessionKey, organizationID, projectID}
	var missing []string
	for _, key := range required {
		if !c.config.Has(key) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config keys: %s", strings.Join(missing, ", "))
	}
	return nil
}

// makeRequest performs an HTTP request to the Claude API with timeout and retry
func (c *Client) makeRequest(method, path string, body interface{}) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	// Set up the request
	req, err := http.NewRequest(method, baseURL+"/api"+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	headers := map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:129.0) Gecko/20100101 Firefox/129.0",
		// NB: Setting this particular Accept-Encoding because Claude will 403 when
		// under heavy load (funny http code choice...) when the client doesn't
		// explicitly state it accepts compressed payloads. Golang's HTTP client
		// default behavior, setting "Accept-Encoding: gzip" also doesn't work
		// (yet another funny Anthropic API quirk...), but this particular header
		// value seems to always do the trick. Finding this value was a happy
		// coincidence to discover — it's what the ruby http client does by default
		// (sandworm was originally written in ruby).
		"Accept-Encoding": "gzip;q=1.0, identity;q=0.3",
		"Cookie":          fmt.Sprintf("sessionKey=%s", c.config.Get(sessionKey)),
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// Retry logic for transient errors
	maxRetries := 3
	var respBody []byte
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			jitter := time.Duration(int64(float64(backoff) * 0.1 * (float64(2*time.Now().UnixNano()%100) / 100)))
			time.Sleep(backoff + jitter)
		}

		// Create a context with timeout
		ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
		defer cancel()

		// Execute request with timeout context
		resp, err := c.httpClient.Do(req.WithContext(ctx))
		if err != nil {
			lastErr = fmt.Errorf("request failed (attempt %d/%d): %w", attempt+1, maxRetries, err)
			continue // Retry on network errors
		}

		// Process the response
		defer resp.Body.Close()

		// Read response body w/ manual decoding
		switch resp.Header.Get("Content-Encoding") {
		case "gzip":
			gz, err := gzip.NewReader(resp.Body)
			if err != nil {
				lastErr = fmt.Errorf("failed to create gzip reader: %w", err)
				continue
			}
			defer gz.Close()
			respBody, err = io.ReadAll(gz)
			if err != nil {
				lastErr = fmt.Errorf("failed to read gzip response: %w", err)
				continue
			}
		default:
			// identity or no encoding
			respBody, err = io.ReadAll(resp.Body)
			if err != nil {
				lastErr = fmt.Errorf("failed to read response body: %w", err)
				continue
			}
		}

		// Check for error status codes that shouldn't be retried
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// Only retry on 5xx errors (server errors) or 429 (rate limit)
			if resp.StatusCode >= 500 || resp.StatusCode == 429 {
				lastErr = fmt.Errorf("API request failed (attempt %d/%d): %d - %s",
					attempt+1, maxRetries, resp.StatusCode, string(respBody))
				continue
			}
			return nil, fmt.Errorf("API request failed: %d - %s", resp.StatusCode, string(respBody))
		}

		// Update session key if it changed
		if cookie := resp.Header.Get("Set-Cookie"); cookie != "" {
			if matches := sessionKeyRegex.FindStringSubmatch(cookie); matches != nil {
				newKey := matches[1]
				if newKey != c.config.Get(sessionKey) {
					if err := c.config.Set(sessionKey, newKey); err != nil {
						return nil, err
					}
				}
			}
		}

		// Success! Return the response
		return respBody, nil
	}

	// If we got here, all retries failed
	return nil, fmt.Errorf("request failed after %d attempts: %w", maxRetries, lastErr)
}

// MARK: Anthropic API requests

func (c *Client) listOrganizations() ([]organization, error) {
	data, err := c.makeRequest(http.MethodGet, "/organizations", nil)
	if err != nil {
		return nil, fmt.Errorf("listOrganizations: %w", err)
	}

	var orgs []organization
	if err := json.Unmarshal(data, &orgs); err != nil {
		return nil, fmt.Errorf("failed to parse organizations: %w", err)
	}
	return orgs, nil
}

func (c *Client) listProjects() ([]project, error) {
	data, err := c.makeRequest(
		http.MethodGet,
		fmt.Sprintf("/organizations/%s/projects", c.config.Get(organizationID)),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("listProjects: %w", err)
	}

	var projects []project
	if err := json.Unmarshal(data, &projects); err != nil {
		return nil, fmt.Errorf("failed to parse projects: %w", err)
	}
	return projects, nil
}

func (c *Client) listDocuments() ([]document, error) {
	data, err := c.makeRequest(
		http.MethodGet,
		fmt.Sprintf(
			"/organizations/%s/projects/%s/docs",
			c.config.Get(organizationID),
			c.config.Get(projectID),
		),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("listDocuments: %w", err)
	}

	var docs []document
	if err := json.Unmarshal(data, &docs); err != nil {
		return nil, fmt.Errorf("failed to parse documents: %w", err)
	}
	return docs, nil
}

func (c *Client) deleteDocument(id string) error {
	_, err := c.makeRequest(
		http.MethodDelete,
		fmt.Sprintf(
			"/organizations/%s/projects/%s/docs/%s",
			c.config.Get(organizationID),
			c.config.Get(projectID),
			id,
		),
		nil,
	)
	if err != nil {
		return fmt.Errorf("deleteDocument: %w", err)
	}
	return nil
}

func (c *Client) uploadDocument(fileName, content string) (*document, error) {
	body := map[string]string{
		"file_name": fileName,
		"content":   content,
	}

	data, err := c.makeRequest(
		http.MethodPost,
		fmt.Sprintf(
			"/organizations/%s/projects/%s/docs",
			c.config.Get(organizationID),
			c.config.Get(projectID),
		),
		body,
	)
	if err != nil {
		return nil, fmt.Errorf("uploadDocument: %w", err)
	}

	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse document: %w", err)
	}
	return &doc, nil
}

// MARK: Anthropic API types

type organization struct {
	ID   string `json:"uuid"`
	Name string `json:"name"`
}

type project struct {
	ID         string    `json:"uuid"`
	Name       string    `json:"name"`
	ArchivedAt time.Time `json:"archived_at,omitempty"`
}

type document struct {
	ID       string `json:"uuid"`
	FileName string `json:"file_name"`
}

// MARK: User interaction (for setup)

// Helper function to present a selection list to the user and return the
// selected item.
func selectFromList[T interface{ GetName() string }](items []T) T {
	for i, item := range items {
		fmt.Printf("%d. %s\n", i+1, item.GetName())
	}

	for {
		fmt.Print("\nEnter selection number: ")
		var input int
		if _, err := fmt.Scanln(&input); err != nil {
			fmt.Println("Invalid input. Please enter a number.")
			continue
		}

		if input < 1 || input > len(items) {
			fmt.Printf("Invalid selection. Please enter a number between 1 and %d\n", len(items))
			continue
		}

		return items[input-1]
	}
}

// GetName implementations for our types to satisfy the generic constraint
func (o organization) GetName() string { return o.Name }
func (p project) GetName() string      { return p.Name }
