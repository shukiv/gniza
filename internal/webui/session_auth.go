package webui

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	cpanelSessionsDir       = "/var/cpanel/sessions/raw"
	cpanelUAPI              = "/usr/local/cpanel/bin/uapi"
	capabilityLifetime      = 20 * time.Second
	teamRoleCheckTimeout    = 5 * time.Second
	maxCPanelSessionSize    = 64 << 10
	maxTeamResponse         = 256 << 10
	capabilityHeader        = "X-Gniza-Capability"
	cpanelAccountHeader     = "X-Gniza-Cpanel-Account"
	cpanelPrincipalHeader   = "X-Gniza-Cpanel-Principal"
	cpanelSessionHeader     = "X-Gniza-Cpanel-Session"
	cpanelSessionKeyHeader  = "X-Gniza-Cpanel-Session-Key"
	cpanelTokenHeader       = "X-Gniza-Cpanel-Token"
	capabilityMethodHeader  = "X-Gniza-Request-Method"
	capabilityTargetHeader  = "X-Gniza-Request-Target"
	capabilityEndpoint      = "/_gniza/capability"
	adminCapabilityEndpoint = "/_gniza/user-capability"
	accountCapabilityScope  = "account-backup-ui"
)

var (
	errSessionDenied      = errors.New("cPanel session was not accepted")
	errSessionUnavailable = errors.New("cPanel session validation is unavailable")
)

// accountSessionAuth turns a real cPanel browser session into a very short
// lived capability for one exact request. The cPanel session is checked by
// the root service, not asserted by the PHP proxy. A consumed nonce cannot be
// replayed against a second restore or download.
type accountSessionAuth struct {
	key         []byte
	sessionsDir string
	rootUID     uint32
	now         func() time.Time
	teamAdmin   func(context.Context, string, string) (bool, error)
	teamSlots   chan struct{}

	mu   sync.Mutex
	used map[string]int64
}

func newAccountSessionAuth(key []byte) *accountSessionAuth {
	return &accountSessionAuth{
		key:         append([]byte(nil), key...),
		sessionsDir: cpanelSessionsDir,
		rootUID:     0,
		now:         time.Now,
		teamAdmin:   cpanelTeamAdministrator,
		teamSlots:   make(chan struct{}, 8),
		used:        map[string]int64{},
	}
}

type capabilityClaims struct {
	Version   int    `json:"v"`
	Account   string `json:"account"`
	Principal string `json:"principal"`
	Scope     string `json:"scope"`
	Method    string `json:"method"`
	Target    string `json:"target"`
	Session   string `json:"session"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	Nonce     string `json:"nonce"`
}

func (a *accountSessionAuth) issue(ctx context.Context, account, sessionName, sessionKey, securityToken, method, target string) (string, error) {
	return a.issueWithProof(ctx, account, "", sessionName, sessionKey, securityToken, method, target, true)
}

// issueForPrincipal is used by cPanel's root-owned AdminBin bridge. The
// principal is derived by cPanel from the live server-side session before it
// reaches this root-only endpoint; the daemon still verifies the same raw
// session name, authenticator and security token independently.
func (a *accountSessionAuth) issueForPrincipal(ctx context.Context, account, principal, sessionName, sessionKey, securityToken, method, target string) (string, error) {
	return a.issueWithProof(ctx, account, principal, sessionName, sessionKey, securityToken, method, target, true)
}

// issueFromCPanelBridge accepts the proof only on the root-owned admin
// socket. cPanel can restore the session from its per-session token there,
// but cannot decrypt pass without the obfuscation fragment in the browser
// cookie that LivePHP deliberately does not receive. The random session name
// and the root-owned record remain independently verified by this daemon.
func (a *accountSessionAuth) issueFromCPanelBridge(ctx context.Context, account, principal, sessionName, securityToken, method, target string) (string, error) {
	return a.issueWithProof(ctx, account, principal, sessionName, "", securityToken, method, target, false)
}

func (a *accountSessionAuth) issueWithProof(ctx context.Context, account, principal, sessionName, sessionKey, securityToken, method, target string, requireSessionKey bool) (string, error) {
	if len(a.key) < 32 || a.now == nil || a.teamAdmin == nil || a.teamSlots == nil {
		return "", errSessionUnavailable
	}
	if !validCapabilityRequest(method, target) {
		return "", errSessionDenied
	}
	principal, err := a.verifyCPanelSessionAs(ctx, account, principal, sessionName, sessionKey, securityToken, requireSessionKey)
	if err != nil {
		return "", err
	}

	nonceBytes := make([]byte, 18)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", errSessionUnavailable
	}
	now := a.now().UTC()
	sessionHash := sha256.Sum256([]byte(sessionName))
	claims := capabilityClaims{
		Version: 1, Account: account, Principal: principal,
		Scope: accountCapabilityScope, Method: method, Target: target,
		Session:   base64.RawURLEncoding.EncodeToString(sessionHash[:]),
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(capabilityLifetime).Unix(),
		Nonce:     base64.RawURLEncoding.EncodeToString(nonceBytes),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", errSessionUnavailable
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write([]byte("v1." + encoded))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return "v1." + encoded + "." + signature, nil
}

func validCapabilityRequest(method, target string) bool {
	if len(target) == 0 || len(target) > 4096 {
		return false
	}
	u, err := url.ParseRequestURI(target)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" {
		return false
	}
	switch method {
	case http.MethodGet:
		return u.Path == "/" || u.Path == "/browse" || u.Path == "/download"
	case http.MethodPost:
		return u.Path == "/restore"
	default:
		return false
	}
}

func (a *accountSessionAuth) consume(account, token, method, target string) error {
	if len(a.key) < 32 || len(token) == 0 || len(token) > 4096 {
		return errSessionDenied
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return errSessionDenied
	}
	provided, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return errSessionDenied
	}
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return errSessionDenied
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errSessionDenied
	}
	var claims capabilityClaims
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claims); err != nil {
		return errSessionDenied
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errSessionDenied
	}
	now := a.now().UTC().Unix()
	if claims.Version != 1 || claims.Account != account || claims.Principal == "" ||
		claims.Scope != accountCapabilityScope || claims.Method != method || claims.Target != target ||
		claims.Session == "" || claims.Nonce == "" || claims.IssuedAt > now+5 ||
		claims.ExpiresAt <= now || claims.ExpiresAt-claims.IssuedAt > int64(capabilityLifetime/time.Second) {
		return errSessionDenied
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for nonce, expiry := range a.used {
		if expiry <= now {
			delete(a.used, nonce)
		}
	}
	if _, replayed := a.used[claims.Nonce]; replayed {
		return errSessionDenied
	}
	a.used[claims.Nonce] = claims.ExpiresAt
	return nil
}

// verifyCPanelSession reads the record cpsrvd itself maintains. The opaque
// session name, authenticator and security token arrive through cPanel's
// root-owned AdminBin bridge; none can be invented merely by sharing the
// cPanel account's Unix uid.
func (a *accountSessionAuth) verifyCPanelSession(ctx context.Context, account, sessionName, sessionKey, securityToken string) (string, error) {
	return a.verifyCPanelSessionAs(ctx, account, "", sessionName, sessionKey, securityToken, true)
}

func (a *accountSessionAuth) verifyCPanelSessionAs(ctx context.Context, account, expectedPrincipal, sessionName, sessionKey, securityToken string, requireSessionKey bool) (string, error) {
	if !plainName(account) || (expectedPrincipal != "" && !validCPanelPrincipal(expectedPrincipal)) ||
		!validCPanelSessionName(sessionName) || (requireSessionKey && !validCPanelSessionKey(sessionKey)) ||
		(!requireSessionKey && sessionKey != "") ||
		!validCPanelSecurityToken(securityToken) {
		return "", errSessionDenied
	}
	file, err := os.Open(a.sessionsDir + "/" + sessionName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errSessionDenied
		}
		return "", errSessionUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", errSessionUnavailable
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != a.rootUID || info.Mode().Perm()&0o022 != 0 ||
		info.Size() <= 0 || info.Size() > maxCPanelSessionSize {
		return "", errSessionDenied
	}
	fields, err := parseCPanelSession(io.LimitReader(file, maxCPanelSessionSize+1))
	if err != nil {
		return "", errSessionDenied
	}
	if fields["cp_security_token"] != securityToken ||
		(requireSessionKey && !hmac.Equal([]byte(fields["pass"]), []byte(sessionKey))) ||
		fields["needs_auth"] == "1" || !cpaneldSession(fields) || !authenticatedSession(fields) {
		return "", errSessionDenied
	}
	if expires := fields["expires"]; expires != "" {
		stamp, err := strconv.ParseInt(expires, 10, 64)
		if err != nil || stamp <= a.now().UTC().Unix() {
			return "", errSessionDenied
		}
	}

	teamUser, err := sessionTeamUser(fields)
	if err != nil {
		return "", errSessionDenied
	}
	recordUser := strings.TrimSpace(fields["user"])
	if expectedPrincipal == "" {
		if recordUser != account {
			return "", errSessionDenied
		}
		expectedPrincipal = account
		if teamUser != "" {
			expectedPrincipal = teamUser
		}
	} else if expectedPrincipal == account {
		if recordUser != account || teamUser != "" {
			return "", errSessionDenied
		}
	} else {
		// Current cPanel releases store a Team session's full login
		// principal in user. Retain the owner+team_user form for older
		// records, but never accept an owner-shaped record with no matching
		// Team marker as a Team login.
		if recordUser != expectedPrincipal &&
			!(recordUser == account && teamUser != "" && sameTeamUser(teamUser, expectedPrincipal)) {
			return "", errSessionDenied
		}
		if teamUser != "" && !sameTeamUser(teamUser, expectedPrincipal) {
			return "", errSessionDenied
		}
		teamUser = expectedPrincipal
	}
	if teamUser == "" {
		return expectedPrincipal, nil
	}
	select {
	case a.teamSlots <- struct{}{}:
		defer func() { <-a.teamSlots }()
	case <-ctx.Done():
		return "", errSessionUnavailable
	}
	allowed, err := a.teamAdmin(ctx, account, teamUser)
	if err != nil {
		return "", errSessionUnavailable
	}
	if !allowed {
		return "", errSessionDenied
	}
	return teamUser, nil
}

func validCPanelPrincipal(principal string) bool {
	if len(principal) == 0 || len(principal) > 256 || strings.ContainsAny(principal, "\x00\r\n") {
		return false
	}
	for _, c := range principal {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

func validCPanelSessionName(name string) bool {
	if len(name) < 12 || len(name) > 256 || name == "." || name == ".." {
		return false
	}
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || strings.ContainsRune("_:+%@.-!#$=?^{}~", c) {
			continue
		}
		return false
	}
	return true
}

func validCPanelSecurityToken(token string) bool {
	if !strings.HasPrefix(token, "/cpsess") || len(token) < 9 || len(token) > 32 {
		return false
	}
	for _, c := range token[len("/cpsess"):] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func validCPanelSessionKey(key string) bool {
	if len(key) < 32 || len(key) > 256 {
		return false
	}
	for _, c := range key {
		if (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || (c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

func parseCPanelSession(r io.Reader) (map[string]string, error) {
	fields := map[string]string{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), maxCPanelSessionSize)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || !validSessionKey(key) {
			return nil, errors.New("malformed cPanel session")
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, errors.New("duplicate cPanel session field")
		}
		fields[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return fields, nil
}

func validSessionKey(key string) bool {
	if key == "" {
		return false
	}
	for i, c := range key {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' ||
			(i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

func cpaneldSession(fields map[string]string) bool {
	if fields["service"] == "cpaneld" || fields["app"] == "cpaneld" {
		return true
	}
	origin := fields["origin_as_string"]
	return strings.Contains(origin, "app=cpaneld")
}

func authenticatedSession(fields map[string]string) bool {
	if fields["successful_internal_auth_with_timestamp"] != "" ||
		fields["successful_external_auth_with_timestamp"] != "" || fields["hasroot"] == "1" {
		return true
	}
	origin := fields["origin_as_string"]
	if strings.Contains(origin, "method=create_user_session") ||
		strings.Contains(origin, "method=handle_auth_transfer") {
		return true
	}
	// WHM's "Log in to cPanel" flow creates a root-possessed temporary
	// account session. cPanel 136 does not put either successful_* marker in
	// that raw record, even though it has completed handle_form_login. Keep
	// this exception deliberately narrow: the record must be root-owned (the
	// caller checks that), bound to cpaneld, registered with cPHulk, possessed
	// by root, and carry cPanel's temporary-user credentials.
	return fields["possessor"] == "root" &&
		fields["hulk_registered"] == "1" &&
		fields["session_temp_user"] != "" &&
		fields["session_temp_pass"] != "" &&
		(fields["session_needs_temp_user"] == "1" || fields["created_session_temp_user"] == "1") &&
		strings.Contains(origin, "app=cpaneld") &&
		strings.Contains(origin, "method=handle_form_login")
}

func sessionTeamUser(fields map[string]string) (string, error) {
	var found string
	for _, key := range []string{"team_user", "teamuser", "team_user_name"} {
		value := strings.TrimSpace(fields[key])
		if value == "" {
			continue
		}
		if found != "" && found != value {
			return "", errors.New("conflicting team principal")
		}
		found = value
	}
	return found, nil
}

// cpanelTeamAdministrator asks the supported Team UAPI for the principal's
// current role. Session data establishes who logged in; this second check
// makes role removal take effect instead of trusting a role cached at login.
func cpanelTeamAdministrator(ctx context.Context, account, teamUser string) (bool, error) {
	checkCtx, cancel := context.WithTimeout(ctx, teamRoleCheckTimeout)
	defer cancel()
	output, err := exec.CommandContext(checkCtx, cpanelUAPI, "--output=json", "--user="+account,
		"Team", "list_team", "format=hash").Output()
	if err != nil {
		return false, fmt.Errorf("query cPanel Team roles: %w", err)
	}
	if len(output) > maxTeamResponse {
		return false, errors.New("cPanel Team response is too large")
	}
	return parseTeamAdministrator(output, teamUser, time.Now())
}

func parseTeamAdministrator(output []byte, teamUser string, now time.Time) (bool, error) {
	var response struct {
		Result struct {
			Data   json.RawMessage `json:"data"`
			Status int             `json:"status"`
			Errors json.RawMessage `json:"errors"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return false, fmt.Errorf("decode cPanel Team response: %w", err)
	}
	if response.Result.Status != 1 || len(response.Result.Data) == 0 || string(response.Result.Data) == "null" {
		return false, errors.New("cPanel Team query failed")
	}
	var data any
	if err := json.Unmarshal(response.Result.Data, &data); err != nil {
		return false, fmt.Errorf("decode cPanel Team data: %w", err)
	}
	entry, found := findTeamEntry(data, teamUser, "")
	if !found {
		return false, nil
	}
	if teamEntryInactive(entry, now) {
		return false, nil
	}
	return hasAdministratorRole(entry["roles"]) || hasAdministratorRole(entry["role"]), nil
}

func findTeamEntry(value any, wanted, mapKey string) (map[string]any, bool) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			if entry, found := findTeamEntry(item, wanted, ""); found {
				return entry, true
			}
		}
	case map[string]any:
		candidate := mapKey
		for _, key := range []string{"user", "username", "team_user", "name"} {
			if text, ok := value[key].(string); ok && text != "" {
				candidate = text
				break
			}
		}
		if sameTeamUser(candidate, wanted) {
			return value, true
		}
		for key, item := range value {
			if entry, found := findTeamEntry(item, wanted, key); found {
				return entry, true
			}
		}
	}
	return nil, false
}

func sameTeamUser(left, right string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		if local, _, found := strings.Cut(value, "@"); found {
			return local
		}
		return value
	}
	return left != "" && normalize(left) == normalize(right)
}

func hasAdministratorRole(value any) bool {
	switch value := value.(type) {
	case string:
		for _, role := range strings.FieldsFunc(strings.ToLower(value), func(c rune) bool {
			return c == ',' || c == ' ' || c == ';'
		}) {
			if role == "admin" || role == "administrator" {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if hasAdministratorRole(item) {
				return true
			}
		}
	case map[string]any:
		for key, item := range value {
			if (strings.EqualFold(key, "admin") || strings.EqualFold(key, "administrator")) && truthy(item) {
				return true
			}
			if (key == "name" || key == "role") && hasAdministratorRole(item) {
				return true
			}
		}
	}
	return false
}

func teamEntryInactive(entry map[string]any, now time.Time) bool {
	for _, key := range []string{"suspended", "is_suspended", "expired", "is_expired", "disabled"} {
		if truthy(entry[key]) {
			return true
		}
	}
	if status, ok := entry["status"].(string); ok {
		switch strings.ToLower(status) {
		case "suspended", "expired", "disabled", "inactive":
			return true
		}
	}
	for _, key := range []string{"expire_date", "expires", "expiration"} {
		if expiry, ok := numericTime(entry[key]); ok && expiry > 0 && expiry <= now.Unix() {
			return true
		}
	}
	return false
}

func truthy(value any) bool {
	switch value := value.(type) {
	case bool:
		return value
	case float64:
		return value != 0
	case string:
		value = strings.ToLower(strings.TrimSpace(value))
		return value == "1" || value == "true" || value == "yes"
	default:
		return false
	}
}

func numericTime(value any) (int64, bool) {
	switch value := value.(type) {
	case float64:
		return int64(value), true
	case string:
		stamp, err := strconv.ParseInt(value, 10, 64)
		return stamp, err == nil
	default:
		return 0, false
	}
}

func (s *Server) issueUserCapability(w http.ResponseWriter, r *http.Request) {
	account := accountOf(r)
	if account == "" {
		accountAttributionError(w)
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, "The capability request must not contain a body.", http.StatusBadRequest)
		return
	}
	if s.userAuth == nil {
		s.log.Error("cPanel session validation unavailable", "account", account)
		http.Error(w, "Gniza could not verify your cPanel session. Ask your host to check the service.", http.StatusServiceUnavailable)
		return
	}
	token, err := s.userAuth.issue(r.Context(), account,
		r.Header.Get(cpanelSessionHeader), r.Header.Get(cpanelSessionKeyHeader), r.Header.Get(cpanelTokenHeader),
		r.Header.Get(capabilityMethodHeader), r.Header.Get(capabilityTargetHeader))
	if err != nil {
		if errors.Is(err, errSessionUnavailable) {
			s.log.Error("cPanel session validation unavailable", "account", account)
			http.Error(w, "Gniza could not verify your cPanel session. Ask your host to check the service.", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "Gniza is available to the account owner and Administrator team users only.", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, token)
}

// issueAdminUserCapability is called only over the root-owned WHM socket by
// cPanel's AdminBin module. AdminBin resolves the current LiveAPI session and
// sends its opaque record proof here; this daemon then validates that proof
// against cPanel's root-owned raw session before minting a one-use token.
func (s *Server) issueAdminUserCapability(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, "The capability request must not contain a body.", http.StatusBadRequest)
		return
	}
	if s.userAuth == nil {
		http.Error(w, "Gniza session validation is unavailable.", http.StatusServiceUnavailable)
		return
	}
	account := r.Header.Get(cpanelAccountHeader)
	principal := r.Header.Get(cpanelPrincipalHeader)
	token, err := s.userAuth.issueFromCPanelBridge(r.Context(), account, principal,
		r.Header.Get(cpanelSessionHeader), r.Header.Get(cpanelTokenHeader),
		r.Header.Get(capabilityMethodHeader), r.Header.Get(capabilityTargetHeader))
	if err != nil {
		if errors.Is(err, errSessionUnavailable) {
			s.log.Error("cPanel AdminBin session validation unavailable", "account", account)
			http.Error(w, "Gniza session validation is unavailable.", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "cPanel session was not accepted.", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, token)
}

func (s *Server) requireUserCapability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := accountOf(r)
		if account == "" {
			accountAttributionError(w)
			return
		}
		// On DirectAdmin the plugin carries the session itself, and
		// there is no capability to consume: the check is the session,
		// put to DirectAdmin on every request. See
		// requireDirectAdminAccount.
		if s.daSessions != nil {
			proven, ok := s.requireDirectAdminAccount(w, r, account)
			if !ok {
				return
			}
			next.ServeHTTP(w, proven)
			return
		}
		if s.userAuth == nil {
			http.Error(w, "Open Gniza from your current cPanel session.", http.StatusForbidden)
			return
		}
		if err := s.userAuth.consume(account, r.Header.Get(capabilityHeader), r.Method, r.URL.RequestURI()); err != nil {
			http.Error(w, "Open Gniza from your current cPanel session.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func accountAttributionError(w http.ResponseWriter) {
	http.Error(w, "Gniza could not tell which account this is. Open it from inside cPanel.", http.StatusForbidden)
}
