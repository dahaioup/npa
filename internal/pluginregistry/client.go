package pluginregistry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// DefaultRegistryURL points at the public NPA plugin repository hosted on the
// community Gitea instance.
const DefaultRegistryURL = "http://gitea.example.com/manman/npa-plugins/raw/branch/main/registry.json"

// downloadAttempts is the number of times a broken download is retried before
// the installer gives up.
const downloadAttempts = 5

// Client reads the registry document from a remote repository.
type Client struct {
	// RegistryURL is the absolute URL of the registry.json document.
	RegistryURL string
	// HTTP is the client used for the request. A short timeout client is
	// used when it is nil.
	HTTP *http.Client
}

// NewClient returns a client for the given registry URL.
func NewClient(registryURL string) *Client {
	if registryURL == "" {
		registryURL = DefaultRegistryURL
	}
	return &Client{RegistryURL: registryURL}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// Fetch downloads and parses the registry document.
func (c *Client) Fetch(ctx context.Context) (*Registry, error) {
	data, err := c.FetchRaw(ctx)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// FetchRaw downloads the registry document without parsing it.
func (c *Client) FetchRaw(ctx context.Context) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.RegistryURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch plugin registry: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch plugin registry: unexpected status %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read plugin registry: %w", err)
	}
	return data, nil
}

// Downloader fetches release assets over HTTP. Interrupted downloads resume
// from the already written bytes, because the public mirrors drop long
// connections regularly.
type Downloader struct {
	// HTTP is the client used for the request.
	HTTP *http.Client
	// Progress is called with the number of bytes written so far.
	Progress func(downloaded int64, total int64)
}

// Download stores the artifact at destination and verifies the size when the
// server announces one.
func (d *Downloader) Download(ctx context.Context, url string, destination string, expectedSize int64) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		written := int64(0)
		if info, err := os.Stat(destination); err == nil {
			written = info.Size()
		}
		if expectedSize > 0 && written >= expectedSize {
			return nil
		}
		if attempt > 1 {
			delay := time.Duration(attempt) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		size, err := d.downloadOnce(ctx, url, destination, written, expectedSize)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("plugin asset download interrupted, retrying", "url", url, "attempt", attempt, "written", size, "error", err)
	}
	return fmt.Errorf("download %s: giving up after %d attempts", url, downloadAttempts)
}

func (d *Downloader) downloadOnce(ctx context.Context, url string, destination string, offset int64, expectedSize int64) (int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return offset, err
	}
	if offset > 0 {
		request.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	client := d.HTTP
	if client == nil {
		client = &http.Client{}
	}
	response, err := client.Do(request)
	if err != nil {
		return offset, err
	}
	defer response.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch response.StatusCode {
	case http.StatusOK:
		flags |= os.O_TRUNC
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	default:
		return offset, fmt.Errorf("download %s returned %s", url, response.Status)
	}

	file, err := os.OpenFile(destination, flags, 0o644)
	if err != nil {
		return offset, err
	}
	writer := &progressWriter{writer: file, total: expectedSize, base: offset}
	if d.Progress != nil {
		writer.callback = d.Progress
	}
	copied, err := io.Copy(writer, response.Body)
	closeErr := file.Close()
	if err != nil {
		return offset + copied, err
	}
	if closeErr != nil {
		return offset + copied, closeErr
	}
	if expectedSize > 0 && offset+copied != expectedSize {
		return offset + copied, fmt.Errorf("download incomplete: got %d bytes, want %d", offset+copied, expectedSize)
	}
	return offset + copied, nil
}

type progressWriter struct {
	writer     io.Writer
	total      int64
	downloaded int64
	base       int64
	callback   func(downloaded int64, total int64)
}

func (w *progressWriter) Write(data []byte) (int, error) {
	written, err := w.writer.Write(data)
	w.downloaded += int64(written)
	if w.callback != nil {
		w.callback(w.base+w.downloaded, w.total)
	}
	return written, err
}
