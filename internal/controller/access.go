package controller

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// Phase 7: who may use the controller. Three ways in:
//
//   - the admin password (HTTP Basic), as before;
//   - API tokens (Authorization: Bearer ...) for scripts and CI, each with a
//     role, created and revoked on the Settings page or with the CLI;
//   - single sign-on through any OpenID Connect provider (Okta, Entra ID,
//     Keycloak, Google, Authentik ...): the browser is sent to the provider,
//     comes back with an ID token, and gets a session cookie. Who becomes
//     admin or viewer is decided by email address or group claim.
//
// Two roles: admin does everything, viewer reads everything (runs, hosts,
// reports, Grafana link) and changes nothing.

const schemaAccess = `
CREATE TABLE IF NOT EXISTS settings (
	key        TEXT PRIMARY KEY,
	value      JSONB NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS api_tokens (
	id         BIGSERIAL PRIMARY KEY,
	name       TEXT NOT NULL,
	token_hash TEXT UNIQUE NOT NULL,
	role       TEXT NOT NULL DEFAULT 'admin',
	prefix     TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	last_used  TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT PRIMARY KEY,
	subject    TEXT NOT NULL,
	name       TEXT NOT NULL DEFAULT '',
	email      TEXT NOT NULL DEFAULT '',
	role       TEXT NOT NULL,
	ip         TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	expires_at TIMESTAMPTZ NOT NULL,
	last_seen  TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// Settings: one JSON document per key (oidc, notify, enrol, updates, backup).

func (s *Store) Setting(ctx context.Context, key string, v any) (bool, error) {
	var b []byte
	err := s.db.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&b)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return false, nil
		}
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func (s *Store) SaveSetting(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, key, b)
	return err
}

func (s *Store) Settings(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := s.db.Query(ctx, `SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = json.RawMessage(v)
	}
	return out, rows.Err()
}

// API tokens.

func (s *Store) CreateAPIToken(ctx context.Context, name, role, hash, prefix string) (api.APIToken, error) {
	t := api.APIToken{Name: name, Role: role, Prefix: prefix}
	err := s.db.QueryRow(ctx, `INSERT INTO api_tokens (name, role, token_hash, prefix) VALUES ($1, $2, $3, $4) RETURNING id, created_at`,
		name, role, hash, prefix).Scan(&t.ID, &t.CreatedAt)
	return t, err
}

func (s *Store) APITokens(ctx context.Context) ([]api.APIToken, error) {
	rows, err := s.db.Query(ctx, `SELECT id, name, role, prefix, created_at, last_used FROM api_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.APIToken{}
	for rows.Next() {
		var t api.APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Role, &t.Prefix, &t.CreatedAt, &t.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) APITokenByHash(ctx context.Context, hash string) (api.APIToken, error) {
	var t api.APIToken
	err := s.db.QueryRow(ctx, `SELECT id, name, role, prefix, created_at, last_used FROM api_tokens WHERE token_hash=$1`, hash).
		Scan(&t.ID, &t.Name, &t.Role, &t.Prefix, &t.CreatedAt, &t.LastUsed)
	return t, err
}

func (s *Store) TouchAPIToken(ctx context.Context, id int64) {
	s.db.Exec(ctx, `UPDATE api_tokens SET last_used=now() WHERE id=$1`, id)
}

func (s *Store) DeleteAPIToken(ctx context.Context, id int64) (bool, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM api_tokens WHERE id=$1`, id)
	return tag.RowsAffected() > 0, err
}

// Sessions (SSO). The cookie value is random; its SHA-256 is the row id.

func (s *Store) CreateSession(ctx context.Context, se api.Session) error {
	_, err := s.db.Exec(ctx, `INSERT INTO sessions (id, subject, name, email, role, ip, created_at, expires_at, last_seen) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$7)`,
		se.ID, se.Subject, se.Name, se.Email, se.Role, se.IP, se.CreatedAt, se.ExpiresAt)
	return err
}

func (s *Store) SessionByID(ctx context.Context, id string) (api.Session, error) {
	var se api.Session
	err := s.db.QueryRow(ctx, `SELECT id, subject, name, email, role, ip, created_at, expires_at, last_seen FROM sessions WHERE id=$1 AND expires_at > now()`, id).
		Scan(&se.ID, &se.Subject, &se.Name, &se.Email, &se.Role, &se.IP, &se.CreatedAt, &se.ExpiresAt, &se.LastSeen)
	return se, err
}

func (s *Store) Sessions(ctx context.Context) ([]api.Session, error) {
	s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	rows, err := s.db.Query(ctx, `SELECT id, subject, name, email, role, ip, created_at, expires_at, last_seen FROM sessions ORDER BY last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Session{}
	for rows.Next() {
		var se api.Session
		if err := rows.Scan(&se.ID, &se.Subject, &se.Name, &se.Email, &se.Role, &se.IP, &se.CreatedAt, &se.ExpiresAt, &se.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, se)
	}
	return out, rows.Err()
}

func (s *Store) TouchSession(ctx context.Context, id string) {
	s.db.Exec(ctx, `UPDATE sessions SET last_seen=now() WHERE id=$1`, id)
}

func (s *Store) DeleteSession(ctx context.Context, id string) (bool, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE id=$1`, id)
	return tag.RowsAffected() > 0, err
}

// OIDCSettings is the "oidc" setting: how to reach the provider and who
// gets which role.
type OIDCSettings struct {
	Enabled         bool     `json:"enabled"`
	ProviderName    string   `json:"provider_name"` // label on the sign-in button
	Issuer          string   `json:"issuer"`        // https://login.example.com/realms/lab (serves /.well-known/openid-configuration)
	ClientID        string   `json:"client_id"`
	ClientSecret    string   `json:"client_secret,omitempty"` // never returned by the API; "" on save keeps the stored one
	ClientSecretSet bool     `json:"client_secret_set,omitempty"`
	Scopes          string   `json:"scopes"`       // default "openid profile email"
	GroupsClaim     string   `json:"groups_claim"` // default "groups"
	Admins          []string `json:"admins"`       // email addresses or group names
	Viewers         []string `json:"viewers"`
	AllowAllViewers bool     `json:"allow_all_viewers"`      // anyone the provider signs in is a viewer
	SessionHours    int      `json:"session_hours"`          // default 12
	RedirectURL     string   `json:"redirect_url,omitempty"` // read-only: what to register at the provider
}

func (o *OIDCSettings) defaults() {
	if o.Scopes == "" {
		o.Scopes = "openid profile email"
	}
	if o.GroupsClaim == "" {
		o.GroupsClaim = "groups"
	}
	if o.SessionHours <= 0 || o.SessionHours > 24*30 {
		o.SessionHours = 12
	}
	if o.ProviderName == "" {
		o.ProviderName = "single sign-on"
	}
	if o.Admins == nil {
		o.Admins = []string{}
	}
	if o.Viewers == nil {
		o.Viewers = []string{}
	}
}

// Ready says the provider is configured enough to send a browser to it.
func (o OIDCSettings) Ready() bool {
	return o.Enabled && o.Issuer != "" && o.ClientID != "" && o.ClientSecret != ""
}

// roleFor maps the ID token's claims to a role: a listed admin wins, then a
// listed viewer, then "anyone signed in is a viewer" if allowed. Lists hold
// email addresses or group names; matching ignores case.
func (o OIDCSettings) roleFor(email string, groups []string) string {
	in := func(list []string) bool {
		for _, want := range list {
			if want == "" {
				continue
			}
			if strings.EqualFold(want, email) {
				return true
			}
			for _, g := range groups {
				if strings.EqualFold(want, g) {
					return true
				}
			}
		}
		return false
	}
	switch {
	case in(o.Admins):
		return "admin"
	case in(o.Viewers), o.AllowAllViewers:
		return "viewer"
	}
	return ""
}

// principal is who a request comes from, after authentication.
type principal struct {
	Name, Role, Via string
	SessionID       string // SSO sessions: the row id, for sign-out
}

type principalKey struct{}

func who(r *http.Request) principal {
	p, _ := r.Context().Value(principalKey{}).(principal)
	return p
}

// access holds the runtime state of Phase 7 authentication.
type access struct {
	mu       sync.Mutex
	oidc     OIDCSettings
	tokens   map[string]api.APIToken // token hash -> record
	touched  map[string]time.Time    // token hash / session id -> last touch written
	sessions map[string]api.Session  // session id -> record
	pending  map[string]oidcPending  // state -> what the callback must match
	disc     *oidcDiscovery
	discAt   time.Time
	discFor  string
	jwks     map[string]json.RawMessage // kid -> key
	jwksAt   time.Time
	httpc    *http.Client
}

type oidcPending struct {
	nonce, verifier, next string
	at                    time.Time
}

type oidcDiscovery struct {
	Issuer        string `json:"issuer"`
	AuthEndpoint  string `json:"authorization_endpoint"`
	TokenEndpoint string `json:"token_endpoint"`
	JWKSURI       string `json:"jwks_uri"`
	UserinfoURI   string `json:"userinfo_endpoint"`
}

func newAccess() *access {
	return &access{tokens: map[string]api.APIToken{}, touched: map[string]time.Time{}, sessions: map[string]api.Session{},
		pending: map[string]oidcPending{}, httpc: &http.Client{Timeout: 15 * time.Second}}
}

// loadAccess reads the OIDC settings on start and after a save.
func (s *Server) loadAccess(ctx context.Context) {
	if s.store == nil {
		return
	}
	var o OIDCSettings
	if _, err := s.store.Setting(ctx, "oidc", &o); err != nil {
		log.Printf("access: oidc setting: %v", err)
	}
	o.defaults()
	s.acc.mu.Lock()
	s.acc.oidc = o
	s.acc.tokens = map[string]api.APIToken{}
	s.acc.sessions = map[string]api.Session{}
	s.acc.mu.Unlock()
}

func (s *Server) oidcSettings() OIDCSettings {
	s.acc.mu.Lock()
	defer s.acc.mu.Unlock()
	return s.acc.oidc
}

// authenticate works out who is calling, or reports nobody.
func (s *Server) authenticate(r *http.Request) (principal, bool) {
	if user, pass, ok := r.BasicAuth(); ok {
		if s.adminLogin(user, pass) {
			return principal{Name: user, Role: "admin", Via: "password"}, true
		}
		return principal{}, false
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && tok != "" {
		hash := hashSecret(tok)
		s.acc.mu.Lock()
		t, cached := s.acc.tokens[hash]
		s.acc.mu.Unlock()
		if !cached {
			if s.store == nil {
				return principal{}, false
			}
			var err error
			if t, err = s.store.APITokenByHash(r.Context(), hash); err != nil {
				return principal{}, false
			}
			s.acc.mu.Lock()
			s.acc.tokens[hash] = t
			s.acc.mu.Unlock()
		}
		s.touch(hash, func() { s.store.TouchAPIToken(context.Background(), t.ID) })
		return principal{Name: "token " + t.Name, Role: t.Role, Via: "token"}, true
	}
	if c, err := r.Cookie("srnet_session"); err == nil && c.Value != "" {
		id := hashSecret(c.Value)
		s.acc.mu.Lock()
		se, cached := s.acc.sessions[id]
		s.acc.mu.Unlock()
		if !cached {
			if s.store == nil {
				return principal{}, false
			}
			var err error
			if se, err = s.store.SessionByID(r.Context(), id); err != nil {
				return principal{}, false
			}
			s.acc.mu.Lock()
			s.acc.sessions[id] = se
			s.acc.mu.Unlock()
		}
		if time.Now().After(se.ExpiresAt) {
			s.acc.mu.Lock()
			delete(s.acc.sessions, id)
			s.acc.mu.Unlock()
			return principal{}, false
		}
		s.touch(id, func() { s.store.TouchSession(context.Background(), id) })
		name := se.Name
		if name == "" {
			name = se.Email
		}
		return principal{Name: name, Role: se.Role, Via: "sso", SessionID: id}, true
	}
	return principal{}, false
}

// touch writes a last-used stamp at most once a minute per credential.
func (s *Server) touch(key string, write func()) {
	if s.store == nil {
		return
	}
	s.acc.mu.Lock()
	last := s.acc.touched[key]
	due := time.Since(last) > time.Minute
	if due {
		s.acc.touched[key] = time.Now()
	}
	s.acc.mu.Unlock()
	if due {
		go write()
	}
}

// require wraps a handler for callers of at least the given role.
func (s *Server) require(role string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.authenticate(r)
		if !ok {
			s.unauthorized(w, r)
			return
		}
		if role == "admin" && p.Role != "admin" {
			httpError(w, http.StatusForbidden, "read-only access: this needs an admin login")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	}
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc { return s.require("admin", h) }
func (s *Server) view(h http.HandlerFunc) http.HandlerFunc  { return s.require("viewer", h) }

// unauthorized answers a request nobody signed. The UI page goes to the
// sign-in page when single sign-on is set up (unless the caller asked for
// the password prompt with ?basic); everything else gets the Basic
// challenge, which makes a browser ask for the admin password.
func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" && s.oidcSettings().Ready() && !r.URL.Query().Has("basic") {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="srnet"`)
	httpError(w, http.StatusUnauthorized, "login required")
}

// Routes.

func (s *Server) routesAccess(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("GET /auth/oidc/start", s.oidcStart)
	mux.HandleFunc("GET /auth/oidc/callback", s.oidcCallback)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /api/v1/whoami", s.view(func(w http.ResponseWriter, r *http.Request) {
		p := who(r)
		writeJSON(w, api.Whoami{Name: p.Name, Role: p.Role, Via: p.Via})
	}))
	mux.HandleFunc("GET /api/v1/access/oidc", s.admin(s.getOIDC))
	mux.HandleFunc("PUT /api/v1/access/oidc", s.admin(s.putOIDC))
	mux.HandleFunc("POST /api/v1/access/oidc/test", s.admin(s.testOIDC))
	mux.HandleFunc("GET /api/v1/access/tokens", s.admin(s.listAPITokens))
	mux.HandleFunc("POST /api/v1/access/tokens", s.admin(s.createAPIToken))
	mux.HandleFunc("DELETE /api/v1/access/tokens/{id}", s.admin(s.deleteAPIToken))
	mux.HandleFunc("GET /api/v1/access/sessions", s.admin(s.listSessions))
	mux.HandleFunc("DELETE /api/v1/access/sessions/{id}", s.admin(s.deleteSession))
}

func (s *Server) getOIDC(w http.ResponseWriter, r *http.Request) {
	o := s.oidcSettings()
	o.ClientSecretSet = o.ClientSecret != ""
	o.ClientSecret = ""
	o.RedirectURL = s.redirectURL()
	writeJSON(w, o)
}

func (s *Server) redirectURL() string {
	return strings.TrimRight(s.cfg.PublicURL, "/") + "/auth/oidc/callback"
}

func (s *Server) putOIDC(w http.ResponseWriter, r *http.Request) {
	var o OIDCSettings
	if !readJSON(w, r, &o) {
		return
	}
	cur := s.oidcSettings()
	if o.ClientSecret == "" {
		o.ClientSecret = cur.ClientSecret
	}
	o.Issuer = strings.TrimRight(strings.TrimSpace(o.Issuer), "/")
	o.ClientID = strings.TrimSpace(o.ClientID)
	o.Admins, o.Viewers = cleanList(o.Admins), cleanList(o.Viewers)
	o.defaults()
	if o.Enabled && (o.Issuer == "" || o.ClientID == "" || o.ClientSecret == "") {
		httpError(w, http.StatusBadRequest, "issuer, client id and client secret are needed before sign-on can be enabled")
		return
	}
	if o.Enabled && !strings.HasPrefix(o.Issuer, "https://") && !strings.HasPrefix(o.Issuer, "http://") {
		httpError(w, http.StatusBadRequest, "issuer must be an https URL")
		return
	}
	if o.Enabled && len(o.Admins) == 0 && len(o.Viewers) == 0 && !o.AllowAllViewers {
		httpError(w, http.StatusBadRequest, "say who may sign in: list admins or viewers, or allow everyone as a viewer")
		return
	}
	if err := s.store.SaveSetting(r.Context(), "oidc", o); err != nil {
		serverError(w, err)
		return
	}
	s.store.Audit(r.Context(), "access.oidc", map[string]any{"enabled": o.Enabled, "issuer": o.Issuer, "admins": o.Admins, "by": who(r).Name})
	s.acc.mu.Lock()
	s.acc.oidc = o
	s.acc.disc, s.acc.jwks = nil, nil
	s.acc.mu.Unlock()
	s.getOIDC(w, r)
}

func cleanList(in []string) []string {
	out := []string{}
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// testOIDC fetches the provider's discovery document with the settings as
// sent (the stored secret fills in when the form leaves it blank), so the
// page can show the endpoints before the settings are saved.
func (s *Server) testOIDC(w http.ResponseWriter, r *http.Request) {
	var o OIDCSettings
	if !readJSON(w, r, &o) {
		return
	}
	o.Issuer = strings.TrimRight(strings.TrimSpace(o.Issuer), "/")
	if o.Issuer == "" {
		httpError(w, http.StatusBadRequest, "issuer is empty")
		return
	}
	d, err := s.acc.discover(r.Context(), o.Issuer, false)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"issuer": d.Issuer, "authorization_endpoint": d.AuthEndpoint, "token_endpoint": d.TokenEndpoint, "jwks_uri": d.JWKSURI,
		"redirect_url": s.redirectURL(), "ok": d.AuthEndpoint != "" && d.TokenEndpoint != ""})
}

func (s *Server) listAPITokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.store.APITokens(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, ts)
}

func (s *Server) createAPIToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 80 {
		httpError(w, http.StatusBadRequest, "give the token a name (what will use it)")
		return
	}
	if req.Role == "" {
		req.Role = "admin"
	}
	if req.Role != "admin" && req.Role != "viewer" {
		httpError(w, http.StatusBadRequest, "role must be admin or viewer")
		return
	}
	tok := "srnet_" + randomToken(30)
	t, err := s.store.CreateAPIToken(r.Context(), req.Name, req.Role, hashSecret(tok), tok[:12])
	if err != nil {
		serverError(w, err)
		return
	}
	s.store.Audit(r.Context(), "access.token.create", map[string]any{"id": t.ID, "name": t.Name, "role": t.Role, "by": who(r).Name})
	t.Token = tok
	writeJSON(w, t)
}

func (s *Server) deleteAPIToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	ok, err := s.store.DeleteAPIToken(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		httpError(w, http.StatusNotFound, "no such token")
		return
	}
	s.acc.mu.Lock()
	for h, t := range s.acc.tokens {
		if t.ID == id {
			delete(s.acc.tokens, h)
		}
	}
	s.acc.mu.Unlock()
	s.store.Audit(r.Context(), "access.token.revoke", map[string]any{"id": id, "by": who(r).Name})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	ses, err := s.store.Sessions(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	me := who(r).SessionID
	for i := range ses {
		ses[i].Current = ses[i].ID == me
	}
	writeJSON(w, ses)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ok, err := s.store.DeleteSession(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		httpError(w, http.StatusNotFound, "no such session")
		return
	}
	s.acc.mu.Lock()
	delete(s.acc.sessions, id)
	s.acc.mu.Unlock()
	s.store.Audit(r.Context(), "access.session.revoke", map[string]any{"id": id[:8], "by": who(r).Name})
	w.WriteHeader(http.StatusNoContent)
}

// logout ends an SSO session. A password login has no session to end: the
// browser keeps sending the password until it is closed.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("srnet_session"); err == nil && c.Value != "" {
		id := hashSecret(c.Value)
		if s.store != nil {
			s.store.DeleteSession(r.Context(), id)
		}
		s.acc.mu.Lock()
		delete(s.acc.sessions, id)
		s.acc.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "srnet_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// loginPage offers the sign-on button and the password fallback.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	o := s.oidcSettings()
	msg := ""
	if e := r.URL.Query().Get("error"); e != "" {
		msg = `<p class="err">` + html.EscapeString(e) + `</p>`
	}
	sso := ""
	if o.Ready() {
		sso = `<a class="btn" href="/auth/oidc/start">Sign in with ` + html.EscapeString(o.ProviderName) + `</a>`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sign in</title>
<style>:root{color-scheme:light dark}body{font:15px/1.5 system-ui,sans-serif;margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:#f4f6fa;color:#111}
@media(prefers-color-scheme:dark){body{background:#14171c;color:#e6e8ec}.card{background:#1d2128!important;border-color:#2c313a!important}.alt{color:#9aa3b2!important}}
.card{background:#fff;border:1px solid #e3e6ec;border-radius:12px;padding:32px 36px;width:min(92vw,360px);box-shadow:0 8px 30px rgba(0,0,0,.08);text-align:center}
h1{font-size:18px;margin:0 0 4px}.sub{color:#6b7280;margin:0 0 22px}.btn{display:block;background:#2a78d6;color:#fff;text-decoration:none;padding:11px 16px;border-radius:8px;font-weight:600;margin-bottom:12px}
.alt{display:block;color:#6b7280;font-size:13px;text-decoration:underline}.err{color:#c62828;background:rgba(198,40,40,.08);border-radius:8px;padding:8px 12px;font-size:13px}</style></head>
<body><div class="card"><h1>SR-Net speed test</h1><p class="sub">Controller sign-in</p>%s%s<a class="alt" href="/?basic=1">Use the admin password instead</a></div></body></html>`, msg, sso)
}

// OIDC: authorization code flow with PKCE.

func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	o := s.oidcSettings()
	if !o.Ready() {
		http.Redirect(w, r, "/login?error="+url.QueryEscape("single sign-on is not set up"), http.StatusFound)
		return
	}
	d, err := s.acc.discover(r.Context(), o.Issuer, true)
	if err != nil {
		http.Redirect(w, r, "/login?error="+url.QueryEscape("provider discovery failed: "+err.Error()), http.StatusFound)
		return
	}
	state, nonce, verifier := randomToken(24), randomToken(24), randomToken(48)
	s.acc.mu.Lock()
	for k, p := range s.acc.pending {
		if time.Since(p.at) > 10*time.Minute {
			delete(s.acc.pending, k)
		}
	}
	s.acc.pending[state] = oidcPending{nonce: nonce, verifier: verifier, at: time.Now()}
	s.acc.mu.Unlock()
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type": {"code"}, "client_id": {o.ClientID}, "redirect_uri": {s.redirectURL()}, "scope": {o.Scopes},
		"state": {state}, "nonce": {nonce}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(d.AuthEndpoint, "?") {
		sep = "&"
	}
	http.Redirect(w, r, d.AuthEndpoint+sep+q.Encode(), http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) {
		log.Printf("sso: %s", msg)
		http.Redirect(w, r, "/login?error="+url.QueryEscape(msg), http.StatusFound)
	}
	o := s.oidcSettings()
	if !o.Ready() {
		fail("single sign-on is not set up")
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		fail("provider said: " + e + " " + r.URL.Query().Get("error_description"))
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	s.acc.mu.Lock()
	p, ok := s.acc.pending[state]
	delete(s.acc.pending, state)
	s.acc.mu.Unlock()
	if !ok || code == "" || time.Since(p.at) > 10*time.Minute {
		fail("sign-in took too long or did not start here; try again")
		return
	}
	d, err := s.acc.discover(r.Context(), o.Issuer, true)
	if err != nil {
		fail("provider discovery failed: " + err.Error())
		return
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {s.redirectURL()}, "code_verifier": {p.verifier},
		"client_id": {o.ClientID}, "client_secret": {o.ClientSecret}}
	req, _ := http.NewRequestWithContext(r.Context(), "POST", d.TokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.acc.httpc.Do(req)
	if err != nil {
		fail("token exchange failed: " + err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	json.Unmarshal(body, &tr)
	if resp.StatusCode != 200 || tr.IDToken == "" {
		fail(fmt.Sprintf("token exchange failed: %s %s %s", resp.Status, tr.Error, tr.Desc))
		return
	}
	claims, err := s.acc.verifyIDToken(r.Context(), tr.IDToken, o, d, p.nonce)
	if err != nil {
		fail("id token rejected: " + err.Error())
		return
	}
	email, _ := claims["email"].(string)
	name, _ := claims["name"].(string)
	if name == "" {
		name, _ = claims["preferred_username"].(string)
	}
	sub, _ := claims["sub"].(string)
	var groups []string
	if g, ok := claims[o.GroupsClaim].([]any); ok {
		for _, v := range g {
			if str, ok := v.(string); ok {
				groups = append(groups, str)
			}
		}
	}
	role := o.roleFor(email, groups)
	if role == "" {
		s.store.Audit(r.Context(), "access.sso.denied", map[string]any{"email": email, "sub": sub, "groups": groups})
		fail(fmt.Sprintf("%s signed in, but is not on the admin or viewer list", nonEmpty(email, sub)))
		return
	}
	cookie := randomToken(32)
	now := time.Now()
	se := api.Session{ID: hashSecret(cookie), Subject: sub, Name: name, Email: email, Role: role, IP: clientIP(r), CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(o.SessionHours) * time.Hour), LastSeen: now}
	if err := s.store.CreateSession(r.Context(), se); err != nil {
		fail("session could not be saved: " + err.Error())
		return
	}
	s.acc.mu.Lock()
	s.acc.sessions[se.ID] = se
	s.acc.mu.Unlock()
	s.store.Audit(r.Context(), "access.sso.login", map[string]any{"email": email, "name": name, "role": role, "ip": se.IP})
	http.SetCookie(w, &http.Cookie{Name: "srnet_session", Value: cookie, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
		Expires: se.ExpiresAt})
	http.Redirect(w, r, "/", http.StatusFound)
}

// discover fetches the provider's configuration, cached for an hour.
func (a *access) discover(ctx context.Context, issuer string, cache bool) (*oidcDiscovery, error) {
	a.mu.Lock()
	if cache && a.disc != nil && a.discFor == issuer && time.Since(a.discAt) < time.Hour {
		d := a.disc
		a.mu.Unlock()
		return d, nil
	}
	a.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", req.URL, resp.Status)
	}
	var d oidcDiscovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return nil, fmt.Errorf("discovery document: %w", err)
	}
	if d.AuthEndpoint == "" || d.TokenEndpoint == "" {
		return nil, errors.New("discovery document lacks the authorization or token endpoint")
	}
	if cache {
		a.mu.Lock()
		a.disc, a.discAt, a.discFor = &d, time.Now(), issuer
		a.mu.Unlock()
	}
	return &d, nil
}

// verifyIDToken checks the ID token's signature against the provider's
// JWKS and its issuer, audience, expiry and nonce, and returns its claims.
func (a *access) verifyIDToken(ctx context.Context, raw string, o OIDCSettings, d *oidcDiscovery, nonce string) (map[string]any, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWT")
	}
	hdrB, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var hdr struct{ Alg, Kid string }
	if err := json.Unmarshal(hdrB, &hdr); err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	key, err := a.jwk(ctx, d.JWKSURI, hdr.Kid)
	if err != nil {
		return nil, err
	}
	if err := verifyJWS(hdr.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return nil, err
	}
	payB, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(payB, &claims); err != nil {
		return nil, err
	}
	if iss, _ := claims["iss"].(string); strings.TrimRight(iss, "/") != strings.TrimRight(o.Issuer, "/") && strings.TrimRight(iss, "/") != strings.TrimRight(d.Issuer, "/") {
		return nil, fmt.Errorf("issuer %q is not %q", iss, o.Issuer)
	}
	audOK := false
	switch aud := claims["aud"].(type) {
	case string:
		audOK = aud == o.ClientID
	case []any:
		for _, v := range aud {
			if v == o.ClientID {
				audOK = true
			}
		}
	}
	if !audOK {
		return nil, errors.New("token is for another client id")
	}
	if exp, _ := claims["exp"].(float64); exp == 0 || time.Now().Unix() > int64(exp)+60 {
		return nil, errors.New("token has expired")
	}
	if n, _ := claims["nonce"].(string); n != nonce {
		return nil, errors.New("nonce mismatch")
	}
	return claims, nil
}

// jwk returns the provider's key with the given id, refetching the key set
// when the id is unknown (keys rotate) but at most once a minute.
func (a *access) jwk(ctx context.Context, uri, kid string) (json.RawMessage, error) {
	if uri == "" {
		return nil, errors.New("provider publishes no jwks_uri")
	}
	a.mu.Lock()
	k, ok := a.jwks[kid]
	fresh := time.Since(a.jwksAt) < time.Minute
	a.mu.Unlock()
	if ok || fresh {
		if !ok {
			return nil, fmt.Errorf("unknown signing key %q", kid)
		}
		return k, nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", uri, nil)
	resp, err := a.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	keys := map[string]json.RawMessage{}
	for _, raw := range set.Keys {
		var h struct{ Kid string }
		json.Unmarshal(raw, &h)
		keys[h.Kid] = raw
	}
	a.mu.Lock()
	a.jwks, a.jwksAt = keys, time.Now()
	a.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

// verifyJWS checks an RS256/384/512 or ES256/384/512 signature with a JWK.
func verifyJWS(alg string, jwk json.RawMessage, signed, sig []byte) error {
	var k struct {
		Kty, N, E, Crv, X, Y string
	}
	if err := json.Unmarshal(jwk, &k); err != nil {
		return err
	}
	hashes := map[string]crypto.Hash{"256": crypto.SHA256, "384": crypto.SHA384, "512": crypto.SHA512}
	h, ok := hashes[strings.TrimLeft(alg, "RSEPS")]
	if !ok {
		return fmt.Errorf("unsupported token algorithm %q", alg)
	}
	var digest []byte
	switch h {
	case crypto.SHA256:
		d := sha256.Sum256(signed)
		digest = d[:]
	case crypto.SHA384:
		d := sha512.Sum384(signed)
		digest = d[:]
	default:
		d := sha512.Sum512(signed)
		digest = d[:]
	}
	b64 := func(s string) (*big.Int, error) {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			return nil, err
		}
		return new(big.Int).SetBytes(b), nil
	}
	switch {
	case strings.HasPrefix(alg, "RS") && k.Kty == "RSA":
		n, err := b64(k.N)
		if err != nil {
			return err
		}
		e, err := b64(k.E)
		if err != nil {
			return err
		}
		pub := &rsa.PublicKey{N: n, E: int(e.Int64())}
		if err := rsa.VerifyPKCS1v15(pub, h, digest, sig); err != nil {
			return errors.New("signature does not verify")
		}
		return nil
	case strings.HasPrefix(alg, "ES") && k.Kty == "EC":
		curves := map[string]elliptic.Curve{"P-256": elliptic.P256(), "P-384": elliptic.P384(), "P-521": elliptic.P521()}
		curve, ok := curves[k.Crv]
		if !ok {
			return fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err := b64(k.X)
		if err != nil {
			return err
		}
		y, err := b64(k.Y)
		if err != nil {
			return err
		}
		if len(sig)%2 != 0 {
			return errors.New("bad signature length")
		}
		rr := new(big.Int).SetBytes(sig[:len(sig)/2])
		ss := new(big.Int).SetBytes(sig[len(sig)/2:])
		if !ecdsa.Verify(&ecdsa.PublicKey{Curve: curve, X: x, Y: y}, digest, rr, ss) {
			return errors.New("signature does not verify")
		}
		return nil
	}
	return fmt.Errorf("token algorithm %q does not match key type %q", alg, k.Kty)
}

// shortID is the first bytes of a hash, for logs and the sessions table.
func shortID(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

var _ = hex.EncodeToString
