package controller

import (
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestIdleErrorsSkipTestGrowthAndSelfResolve(t *testing.T) {
	a := newAlerter(nil, newHub(), Config{})
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

	if grew, quiet := a.idleErrors("h|p", 10, at(0), false); grew != 0 || quiet {
		t.Fatalf("first sample: grew %v quiet %v", grew, quiet)
	}
	if grew, _ := a.idleErrors("h|p", 10, at(15), false); grew != 0 {
		t.Fatalf("flat counters alerted: %v", grew)
	}
	// A run on the host pushes the counters up; the first idle sample after it
	// must only record the new level.
	a.idleErrors("h|p", 500, at(30), true)
	a.idleErrors("h|p", 900, at(60), true)
	if grew, quiet := a.idleErrors("h|p", 900, at(90), false); grew != 0 || quiet {
		t.Fatalf("growth during the test was reported as idle: grew %v quiet %v", grew, quiet)
	}
	// Growth while idle is the real signal.
	if grew, _ := a.idleErrors("h|p", 903, at(105), false); grew != 3 {
		t.Fatalf("idle growth: %v", grew)
	}
	// Flat for less than idleQuietFor: still open.
	if grew, quiet := a.idleErrors("h|p", 903, at(105+600), false); grew != 0 || quiet {
		t.Fatalf("resolved too early: grew %v quiet %v", grew, quiet)
	}
	// Flat for idleQuietFor: resolves once.
	if _, quiet := a.idleErrors("h|p", 903, at(105+int(idleQuietFor.Seconds())), false); !quiet {
		t.Fatal("did not resolve after a quiet period")
	}
	if _, quiet := a.idleErrors("h|p", 903, at(105+int(idleQuietFor.Seconds())+15), false); quiet {
		t.Fatal("resolved twice")
	}
	// Another port is independent.
	if grew, _ := a.idleErrors("h|q", 1, at(200), false); grew != 0 {
		t.Fatalf("new port alerted on its first sample: %v", grew)
	}
}

func TestAdminLoginCachesOneCredential(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("hunter22"), bcrypt.MinCost)
	cfg := DefaultConfig()
	cfg.AdminUser, cfg.AdminPasswordHash = "admin", string(hash)
	s := &Server{cfg: cfg}
	if s.adminLogin("admin", "wrong") || s.adminLogin("root", "hunter22") {
		t.Fatal("bad credential accepted")
	}
	if s.adminOKHash != "" {
		t.Fatal("a failed login was cached")
	}
	if !s.adminLogin("admin", "hunter22") {
		t.Fatal("good credential rejected")
	}
	if s.adminOKHash != string(hash) {
		t.Fatal("good credential not cached")
	}
	if !s.adminLogin("admin", "hunter22") || s.adminLogin("admin", "hunter23") {
		t.Fatal("cache answered wrongly")
	}
	// A password change invalidates the cache by itself.
	hash2, _ := bcrypt.GenerateFromPassword([]byte("newpass99"), bcrypt.MinCost)
	s.cfg.AdminPasswordHash = string(hash2)
	if s.adminLogin("admin", "hunter22") {
		t.Fatal("old password still accepted after the hash changed")
	}
	if !s.adminLogin("admin", "newpass99") || s.adminOKHash != string(hash2) {
		t.Fatal("new password rejected or not cached")
	}
}

func TestVersionLessIsNumeric(t *testing.T) {
	ordered := []string{"v0.1.9", "v0.1.18", "v0.1.19-rc1", "v0.1.19", "v0.2.0", "v1.0.0"}
	for i := 0; i+1 < len(ordered); i++ {
		if !versionLess(ordered[i], ordered[i+1]) || versionLess(ordered[i+1], ordered[i]) {
			t.Errorf("%s should sort before %s", ordered[i], ordered[i+1])
		}
	}
}
