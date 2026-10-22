package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestUIAndStatusNeedAdminLogin(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("hunter22"), bcrypt.MinCost)
	cfg := DefaultConfig()
	cfg.AdminUser, cfg.AdminPasswordHash = "admin", string(hash)
	s := &Server{cfg: cfg, hub: newHub(), metrics: newMetrics("")}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	r, _ := http.Get(srv.URL + "/")
	if r.StatusCode != http.StatusUnauthorized || r.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("unauthenticated UI: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.SetBasicAuth("admin", "hunter22")
	r, _ = http.DefaultClient.Do(req)
	body := make([]byte, 4096)
	n, _ := r.Body.Read(body)
	if r.StatusCode != 200 || !strings.Contains(string(body[:n]), "<title>SR-Net-Speed-Test</title>") {
		t.Errorf("UI: %d %q", r.StatusCode, body[:n])
	}
	req, _ = http.NewRequest("GET", srv.URL+"/api/v1/status", nil)
	req.SetBasicAuth("admin", "wrong")
	if r, _ = http.DefaultClient.Do(req); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password accepted: %d", r.StatusCode)
	}
	if r, _ = http.Get(srv.URL + "/healthz"); r.StatusCode != 200 {
		t.Errorf("healthz: %d", r.StatusCode)
	}
	if d, err := GrafanaDashboards(); err != nil || len(d) != 5 {
		t.Errorf("dashboards: %d %v", len(d), err)
	}
}
