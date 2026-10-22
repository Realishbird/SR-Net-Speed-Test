package controller

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// testServer builds a Server without Postgres, with API tokens and OIDC
// settings injected straight into the access cache.
func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.AdminUser, cfg.AdminPasswordHash = "admin", ""
	s := &Server{cfg: cfg, hub: newHub(), metrics: newMetrics(""), acc: newAccess()}
	s.notify = newNotifier(nil, "https://ctl.example")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func doReq(t *testing.T, method, u, bearer string, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	r, err := noRedirect().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAPITokenRoles(t *testing.T) {
	s, srv := testServer(t)
	admin, viewer := "srnet_"+randomToken(30), "srnet_"+randomToken(30)
	s.acc.tokens[hashSecret(admin)] = api.APIToken{ID: 1, Name: "ci", Role: "admin"}
	s.acc.tokens[hashSecret(viewer)] = api.APIToken{ID: 2, Name: "grafana", Role: "viewer"}

	r := doReq(t, "GET", srv.URL+"/api/v1/whoami", "", "")
	if r.StatusCode != 401 {
		t.Fatalf("no credentials: %d", r.StatusCode)
	}
	r = doReq(t, "GET", srv.URL+"/api/v1/whoami", "srnet_nope", "")
	if r.StatusCode != 401 {
		t.Fatalf("unknown token: %d", r.StatusCode)
	}
	var w api.Whoami
	r = doReq(t, "GET", srv.URL+"/api/v1/whoami", viewer, "")
	json.NewDecoder(r.Body).Decode(&w)
	if r.StatusCode != 200 || w.Role != "viewer" || w.Via != "token" || w.Name != "token grafana" {
		t.Fatalf("viewer whoami: %d %+v", r.StatusCode, w)
	}
	if r = doReq(t, "GET", srv.URL+"/api/v1/status", viewer, ""); r.StatusCode != 200 {
		t.Errorf("viewer GET status: %d", r.StatusCode)
	}
	if r = doReq(t, "GET", srv.URL+"/api/v1/access/tokens", viewer, ""); r.StatusCode != 403 {
		t.Errorf("viewer on an admin route: %d", r.StatusCode)
	}
	if r = doReq(t, "POST", srv.URL+"/api/v1/runs", viewer, `{}`); r.StatusCode != 403 {
		t.Errorf("viewer POST: %d", r.StatusCode)
	}
	r = doReq(t, "GET", srv.URL+"/api/v1/whoami", admin, "")
	json.NewDecoder(r.Body).Decode(&w)
	if r.StatusCode != 200 || w.Role != "admin" {
		t.Errorf("admin whoami: %d %+v", r.StatusCode, w)
	}
	var st map[string]any
	r = doReq(t, "GET", srv.URL+"/api/v1/status", admin, "")
	json.NewDecoder(r.Body).Decode(&st)
	if who, _ := st["whoami"].(map[string]any); who["role"] != "admin" || st["sso"] != false {
		t.Errorf("status whoami/sso: %v %v", st["whoami"], st["sso"])
	}
}

func TestLoginPageAndSSORedirect(t *testing.T) {
	s, srv := testServer(t)
	// Without single sign-on the UI gets the password prompt.
	if r := doReq(t, "GET", srv.URL+"/", "", ""); r.StatusCode != 401 || r.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("no sso: %d", r.StatusCode)
	}
	if r := doReq(t, "GET", srv.URL+"/login", "", ""); r.StatusCode != 200 {
		t.Fatalf("login page: %d", r.StatusCode)
	}
	o := OIDCSettings{Enabled: true, ProviderName: "Okta", Issuer: "https://idp.example", ClientID: "abc", ClientSecret: "s"}
	o.defaults()
	s.acc.mu.Lock()
	s.acc.oidc = o
	s.acc.mu.Unlock()
	r := doReq(t, "GET", srv.URL+"/", "", "")
	if r.StatusCode != 302 || r.Header.Get("Location") != "/login" {
		t.Fatalf("sso redirect: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r = doReq(t, "GET", srv.URL+"/?basic", "", ""); r.StatusCode != 401 {
		t.Errorf("?basic should still prompt for the password: %d", r.StatusCode)
	}
	if r = doReq(t, "GET", srv.URL+"/api/v1/status", "", ""); r.StatusCode != 401 {
		t.Errorf("API never redirects: %d", r.StatusCode)
	}
	r = doReq(t, "GET", srv.URL+"/login", "", "")
	b, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(b), "Okta") || !strings.Contains(string(b), "/auth/oidc/start") {
		t.Errorf("login page lacks the provider button: %s", b)
	}
	// A callback that did not start here is bounced back to the login page.
	r = doReq(t, "GET", srv.URL+"/auth/oidc/callback?state=bogus&code=x", "", "")
	if r.StatusCode != 302 || !strings.Contains(r.Header.Get("Location"), "/login?error=") {
		t.Errorf("bogus callback: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
}

func TestRoleFor(t *testing.T) {
	o := OIDCSettings{Admins: []string{"Dylan@Example.com", "net-admins", ""}, Viewers: []string{"Everyone"}}
	cases := []struct {
		email  string
		groups []string
		want   string
	}{
		{"dylan@example.com", nil, "admin"},
		{"ops@example.com", []string{"net-admins"}, "admin"},
		{"ops@example.com", []string{"everyone"}, "viewer"},
		{"ops@example.com", nil, ""},
	}
	for _, c := range cases {
		if got := o.roleFor(c.email, c.groups); got != c.want {
			t.Errorf("roleFor(%s, %v) = %q, want %q", c.email, c.groups, got, c.want)
		}
	}
	o.AllowAllViewers = true
	if got := o.roleFor("anyone@example.com", nil); got != "viewer" {
		t.Errorf("allow all viewers: %q", got)
	}
}

// TestOIDCFlow runs the sign-in against a fake provider: discovery, the
// redirect to its authorize endpoint with PKCE, and an RS256 id token
// checked against its JWKS.
func TestOIDCFlow(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var idp *httptest.Server
	idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": idp.URL, "authorization_endpoint": idp.URL + "/authorize",
				"token_endpoint": idp.URL + "/token", "jwks_uri": idp.URL + "/jwks"})
		case "/jwks":
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer idp.Close()

	s, srv := testServer(t)
	o := OIDCSettings{Enabled: true, Issuer: idp.URL, ClientID: "srnet", ClientSecret: "secret", Admins: []string{"dylan@example.com"}}
	o.defaults()
	s.acc.mu.Lock()
	s.acc.oidc = o
	s.acc.mu.Unlock()

	r := doReq(t, "GET", srv.URL+"/auth/oidc/start", "", "")
	loc, _ := url.Parse(r.Header.Get("Location"))
	if r.StatusCode != 302 || loc == nil || !strings.HasPrefix(loc.String(), idp.URL+"/authorize?") {
		t.Fatalf("start: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	q := loc.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != "srnet" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatalf("authorize query: %v", q)
	}
	if q.Get("redirect_uri") != strings.TrimRight(s.cfg.PublicURL, "/")+"/auth/oidc/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	s.acc.mu.Lock()
	p, ok := s.acc.pending[q.Get("state")]
	s.acc.mu.Unlock()
	if !ok || p.nonce != q.Get("nonce") {
		t.Fatal("pending state not recorded")
	}
	sum := sha256.Sum256([]byte(p.verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") {
		t.Error("code_challenge is not S256 of the verifier")
	}

	sign := func(claims map[string]any) string {
		hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
		pay, _ := json.Marshal(claims)
		signed := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(pay)
		h := sha256.Sum256([]byte(signed))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
		return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	d, err := s.acc.discover(t.Context(), idp.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	good := map[string]any{"iss": idp.URL, "aud": "srnet", "sub": "u1", "email": "dylan@example.com", "name": "Dylan", "nonce": p.nonce, "exp": time.Now().Add(time.Hour).Unix()}
	claims, err := s.acc.verifyIDToken(t.Context(), sign(good), o, d, p.nonce)
	if err != nil || claims["email"] != "dylan@example.com" {
		t.Fatalf("good token rejected: %v %v", err, claims)
	}
	if role := o.roleFor("dylan@example.com", nil); role != "admin" {
		t.Errorf("role = %q", role)
	}
	bad := map[string]map[string]any{
		"wrong audience": {"iss": idp.URL, "aud": "other", "nonce": p.nonce, "exp": good["exp"]},
		"wrong issuer":   {"iss": "https://evil.example", "aud": "srnet", "nonce": p.nonce, "exp": good["exp"]},
		"wrong nonce":    {"iss": idp.URL, "aud": "srnet", "nonce": "x", "exp": good["exp"]},
		"expired":        {"iss": idp.URL, "aud": "srnet", "nonce": p.nonce, "exp": time.Now().Add(-time.Hour).Unix()},
	}
	for name, c := range bad {
		if _, err := s.acc.verifyIDToken(t.Context(), sign(c), o, d, p.nonce); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A token signed by another key fails the signature check.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1"})
	pay, _ := json.Marshal(good)
	signed := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(pay)
	h := sha256.Sum256([]byte(signed))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, other, crypto.SHA256, h[:])
	if _, err := s.acc.verifyIDToken(t.Context(), signed+"."+base64.RawURLEncoding.EncodeToString(sig), o, d, p.nonce); err == nil {
		t.Error("forged signature accepted")
	}
}

func TestRenderNotify(t *testing.T) {
	m := notifyMsg{Event: "run_fail", Level: "fail", Title: "Run 12 failed", Text: "3 of 8 legs under 90%", Link: "https://ctl/#run/12", Fields: [][2]string{{"Aggregate", "612 Gbps"}}}
	var slack map[string]any
	json.Unmarshal(renderNotify("slack", m), &slack)
	att := slack["attachments"].([]any)[0].(map[string]any)
	if slack["text"] != "<https://ctl/#run/12|Run 12 failed>" || att["color"] != "#e34948" || len(att["fields"].([]any)) != 1 {
		t.Errorf("slack: %v", slack)
	}
	var teams map[string]any
	json.Unmarshal(renderNotify("teams", m), &teams)
	card := teams["attachments"].([]any)[0].(map[string]any)["content"].(map[string]any)
	if card["type"] != "AdaptiveCard" || len(card["body"].([]any)) != 3 || len(card["actions"].([]any)) != 1 {
		t.Errorf("teams: %v", card)
	}
	var hook map[string]any
	json.Unmarshal(renderNotify("webhook", m), &hook)
	if hook["event"] != "run_fail" || hook["fields"].(map[string]any)["Aggregate"] != "612 Gbps" || hook["link"] != m.Link {
		t.Errorf("webhook: %v", hook)
	}
}

func TestNotifierSendsToSubscribedChannels(t *testing.T) {
	n := newNotifier(nil, "https://ctl.example/")
	got := make(chan NotifyChannel, 4)
	n.sent = func(c NotifyChannel, body []byte) error { got <- c; return nil }
	err := n.save(t.Context(), []NotifyChannel{
		{Name: "ops", Kind: "slack", URL: "https://hooks.slack.com/x", Events: []string{"run_fail", "alert_open"}, Enabled: true},
		{Name: "all", Kind: "webhook", URL: "https://example/hook", Events: []string{"run_pass", "run_fail"}, Enabled: true},
		{Name: "off", Kind: "teams", URL: "https://example/teams", Events: []string{"run_fail"}, Enabled: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cs := n.channels(); len(cs) != 3 || cs[0].ID != 1 || cs[2].ID != 3 {
		t.Fatalf("ids: %+v", cs)
	}
	if err := n.save(t.Context(), []NotifyChannel{{Name: "x", Kind: "irc", URL: "https://e"}}); err == nil {
		t.Error("bad kind accepted")
	}
	if err := n.save(t.Context(), []NotifyChannel{{Name: "x", Kind: "slack", URL: "hooks.slack.com/x"}}); err == nil {
		t.Error("bad URL accepted")
	}
	n.runFinished(api.Run{ID: 7, Name: "nightly", Verdict: "fail", Status: "done", Summary: api.RunSummary{ToolGbps: 612}})
	names := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case c := <-got:
			names[c.Name] = true
		case <-time.After(5 * time.Second):
			t.Fatal("notification not delivered")
		}
	}
	if !names["ops"] || !names["all"] {
		t.Errorf("delivered to %v", names)
	}
	select {
	case c := <-got:
		t.Errorf("unexpected delivery to %s", c.Name)
	case <-time.After(200 * time.Millisecond):
	}
	for _, c := range n.channels() {
		if c.Name != "off" && (c.Sent != 1 || !c.LastOK) {
			t.Errorf("%s status: %+v", c.Name, c)
		}
	}
	if n.link("#run/7") != "https://ctl.example/#run/7" {
		t.Errorf("link = %q", n.link("#run/7"))
	}
}

func TestWebhookSignature(t *testing.T) {
	var gotSig string
	var gotBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-SRNet-Signature")
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer ts.Close()
	n := newNotifier(nil, "")
	c := NotifyChannel{ID: 1, Name: "h", Kind: "webhook", URL: ts.URL, Secret: "shh"}
	if err := n.deliver(c, notifyMsg{Event: "run_pass", Title: "ok"}); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("shh"))
	mac.Write(gotBody)
	if gotSig != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Errorf("signature %q does not match body", gotSig)
	}
}

func TestNotifyRoutes(t *testing.T) {
	s, srv := testServer(t)
	tok := "srnet_" + randomToken(30)
	s.acc.tokens[hashSecret(tok)] = api.APIToken{ID: 1, Name: "ci", Role: "admin"}
	r := doReq(t, "PUT", srv.URL+"/api/v1/notify", tok, `{"channels":[{"name":"ops","kind":"slack","url":"https://hooks.slack.com/a","events":["run_fail"],"enabled":true}]}`)
	if r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("put: %d %s", r.StatusCode, b)
	}
	var out struct {
		Channels []NotifyChannel `json:"channels"`
		Events   []struct{ Key, Label string }
	}
	r = doReq(t, "GET", srv.URL+"/api/v1/notify", tok, "")
	json.NewDecoder(r.Body).Decode(&out)
	if len(out.Channels) != 1 || out.Channels[0].ID != 1 || len(out.Events) != len(notifyEvents) {
		t.Errorf("get: %+v", out)
	}
}

func TestAcceptanceHTML(t *testing.T) {
	end := time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)
	a := acceptance{
		SuiteRun:   api.SuiteRun{ID: 4, SuiteName: "Nightly", Trigger: "schedule:nightly", Status: "done", Verdict: "pass", RunIDs: []int64{10, 11}, StartedAt: end.Add(-time.Hour), EndedAt: &end},
		Runs:       []api.Run{{ID: 10, Name: "write", Status: "done", Verdict: "pass", Summary: api.RunSummary{ToolGbps: 784}}, {ID: 11, Name: "send", Status: "done", Verdict: "warn", Summary: api.RunSummary{ToolGbps: 1518}}},
		Hosts:      []api.Host{{ID: 1, Name: "dell770ap", Hostname: "dell770ap", Online: true}},
		Controller: "v0.1.25", Generated: end, Site: "https://ctl",
	}
	var b strings.Builder
	WriteAcceptanceHTML(&b, a)
	out := b.String()
	for _, want := range []string{"<!doctype html>", "Nightly", "784", "1518", "dell770ap", "v0.1.25", "Tested by", "Accepted by", "PASS"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Count(out, "<h1") != 1 {
		t.Errorf("report has %d top headings", strings.Count(out, "<h1"))
	}
}

func TestBackupRoundTrip(t *testing.T) {
	b := Backup{Format: 1, CreatedAt: time.Now().UTC(), Hosts: []BackupHost{{UID: "u", Name: "spark-1", Tags: []string{"spark"}, Approved: true,
		Ports: []BackupPort{{RdmaDev: "mlx5_0", PortNum: 1, Netdev: "enp1s0f0np0", Label: "P1", Fabric: "100", ExpectedGbps: 100}}}},
		Settings:  map[string]json.RawMessage{"enrol": json.RawMessage(`{"cidrs":["172.27.0.0/16"]}`)},
		APITokens: []BackupToken{{Name: "ci", Role: "admin", Prefix: "srnet_abc", TokenHash: "h"}}}
	data, _ := json.Marshal(b)
	var back Backup
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Hosts[0].Ports[0].Label != "P1" || string(back.Settings["enrol"]) != `{"cidrs":["172.27.0.0/16"]}` || back.APITokens[0].TokenHash != "h" {
		t.Errorf("round trip: %+v", back)
	}
	if strings.Contains(string(data), `"runs"`) {
		t.Error("empty runs should be omitted")
	}
}
