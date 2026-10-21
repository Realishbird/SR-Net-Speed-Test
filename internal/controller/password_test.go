package controller

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestChangePassword(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	h, _ := bcrypt.GenerateFromPassword([]byte("oldpassword"), bcrypt.MinCost)
	cfg.AdminPasswordHash = string(h)
	path := filepath.Join(dir, "controller.json")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg, hub: newHub(), metrics: newMetrics("")}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/admin/password", s.admin(s.changePassword))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(user, pass, body string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1/admin/password", strings.NewReader(body))
		req.SetBasicAuth(user, pass)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("admin", "wrong", `{"current":"oldpassword","new":"newpassword1"}`); c != 401 {
		t.Errorf("bad login = %d", c)
	}
	if c := post("admin", "oldpassword", `{"current":"nope","new":"newpassword1"}`); c != 403 {
		t.Errorf("wrong current = %d", c)
	}
	if c := post("admin", "oldpassword", `{"current":"oldpassword","new":"short"}`); c != 400 {
		t.Errorf("short new = %d", c)
	}
	if c := post("admin", "oldpassword", `{"current":"oldpassword","new":"newpassword1"}`); c != 204 {
		t.Errorf("change = %d", c)
	}
	if c := post("admin", "oldpassword", `{"current":"oldpassword","new":"newpassword2"}`); c != 401 {
		t.Errorf("old password still accepted: %d", c)
	}
	if c := post("admin", "newpassword1", `{"current":"newpassword1","new":"newpassword2"}`); c != 204 {
		t.Errorf("new password not accepted: %d", c)
	}
	// The change survives a restart: it is in the config file.
	b, _ := os.ReadFile(path)
	again, _ := LoadConfig(path)
	if bcrypt.CompareHashAndPassword([]byte(again.AdminPasswordHash), []byte("newpassword2")) != nil {
		t.Errorf("config on disk does not hold the new hash: %s", b)
	}
}
