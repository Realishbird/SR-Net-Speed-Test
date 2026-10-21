package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// Self-update layout, shared with the launcher script that install.sh writes:
//
//	/var/lib/srnet/bin/srnet-agent        the binary the launcher runs
//	/var/lib/srnet/bin/srnet-agent.next   a verified download, swapped in on next start
//	/var/lib/srnet/bin/srnet-agent.prev   the previous binary, restored if the new one fails
//	/var/lib/srnet/bin/trial              exists while a new binary has not yet proven itself
//
// The agent downloads and verifies; the launcher swaps; systemd restarts.
var binDir = "/var/lib/srnet/bin"

// ApplyUpdate downloads the binary for this host's architecture from the
// controller, checks its SHA-256, stages it for the launcher and returns.
// The caller then exits so systemd restarts the agent through the launcher.
func (c *Client) ApplyUpdate(ctx context.Context, t api.Task) error {
	if t.Version == "" || t.URLPath == "" || len(t.SHA256) != 64 {
		return errors.New("update task is missing version, url or checksum")
	}
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		return err
	}
	tmp := filepath.Join(binDir, "srnet-agent.download")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+t.URLPath, nil)
	if err != nil {
		f.Close()
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		f.Close()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		f.Close()
		return fmt.Errorf("download: %s", resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != t.SHA256 {
		os.Remove(tmp)
		return fmt.Errorf("checksum mismatch: got %s want %s", got, t.SHA256)
	}
	return os.Rename(tmp, filepath.Join(binDir, "srnet-agent.next"))
}

// MarkHealthy tells the launcher the running binary works, so it will not be
// rolled back. Called after the first successful poll.
func MarkHealthy() {
	os.Remove(filepath.Join(binDir, "trial"))
	os.Remove(filepath.Join(binDir, "srnet-agent.prev"))
}
