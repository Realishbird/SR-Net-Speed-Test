package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

func TestApplyUpdateVerifiesChecksumAndStagesNext(t *testing.T) {
	old := binDir
	binDir = t.TempDir()
	defer func() { binDir = old }()

	body := []byte("#!/bin/sh\necho new agent\n")
	sum := sha256.Sum256(body)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write(body)
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, secret: "s3cret", http: srv.Client()}

	bad := api.Task{Kind: api.TaskUpdate, Version: "v0.3.0", URLPath: "/api/v1/agent/release/v0.3.0/amd64", SHA256: hex.EncodeToString(make([]byte, 32))}
	if err := c.ApplyUpdate(context.Background(), bad); err == nil {
		t.Fatal("wrong checksum was accepted")
	}
	if _, err := os.Stat(filepath.Join(binDir, "srnet-agent.next")); err == nil {
		t.Fatal("a binary with a bad checksum was staged")
	}

	good := bad
	good.SHA256 = hex.EncodeToString(sum[:])
	if err := c.ApplyUpdate(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("download auth = %q", gotAuth)
	}
	staged, err := os.ReadFile(filepath.Join(binDir, "srnet-agent.next"))
	if err != nil || string(staged) != string(body) {
		t.Fatalf("staged binary: %v %q", err, staged)
	}
	if fi, _ := os.Stat(filepath.Join(binDir, "srnet-agent.next")); fi.Mode()&0o100 == 0 {
		t.Error("staged binary is not executable")
	}
	if err := c.ApplyUpdate(context.Background(), api.Task{Kind: api.TaskUpdate}); err == nil {
		t.Error("task without version/url/checksum was accepted")
	}
}
