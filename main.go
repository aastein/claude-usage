// Command claude-usage monitors Claude Max subscription usage across multiple
// accounts by polling the account-wide OAuth usage endpoint that powers the
// in-app /usage view. It reads/refreshes OAuth credentials captured from the
// macOS Keychain and renders a live per-account terminal dashboard.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// oauthClientID is Claude Code's public OAuth client id, used for the login and
// token-refresh flows (PKCE, no client secret).
const oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

const (
	usageURL     = "https://api.anthropic.com/api/oauth/usage"
	profileURL   = "https://api.anthropic.com/api/oauth/profile"
	authorizeURL = "https://claude.ai/oauth/authorize"
	tokenURL     = "https://platform.claude.com/v1/oauth/token"
	redirectURI  = "https://platform.claude.com/oauth/code/callback"
	oauthScopes  = "user:profile user:inference user:sessions:claude_code user:mcp_servers"
	oauthBeta    = "oauth-2025-04-20"
	keychainName = "Claude Code-credentials"
	pollInterval = 5 * time.Minute
	// menubarPollInterval is tighter than the TUI's so an active session near
	// its limit is observed in the 98–100% band before it blocks, giving the
	// auto-swap a chance to fire before the API limit is hit.
	menubarPollInterval = 2 * time.Minute
)

// httpClient bounds every request so a stalled connection can't hang the poll loop.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// credential is the OAuth material Claude stores per account.
type credential struct {
	AccessToken           string   `json:"accessToken"`
	RefreshToken          string   `json:"refreshToken"`
	ExpiresAt             int64    `json:"expiresAt"` // unix ms
	RefreshTokenExpiresAt int64    `json:"refreshTokenExpiresAt,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	SubscriptionType      string   `json:"subscriptionType,omitempty"`
}

// account identifies a monitored account by its stable email/uuid and holds its
// OAuth credential. Email is the display identity; AccountUUID matches against
// the currently logged-in Claude Code session.
type account struct {
	Email       string     `json:"email"`
	AccountUUID string     `json:"accountUUID"`
	Credential  credential `json:"credential"`
}

// store is the on-disk config: the set of monitored accounts plus settings.
type store struct {
	Accounts []account `json:"accounts"`
	Settings settings  `json:"settings"`
}

// settings holds user-tunable menu bar behavior.
type settings struct {
	// NotifyThresholdPct is the utilization percent (0–100) at or above which
	// the active account triggers a notification. Applies to both windows.
	NotifyThresholdPct float64 `json:"notifyThresholdPct"`
	// AutoSwapOnAlert, when true, rewrites the Claude Code keychain credential
	// to a healthier account when the active account's SESSION (5h) usage
	// crosses NotifyThresholdPct. Off by default: it silently changes which
	// account subsequent Claude Code requests bill to.
	AutoSwapOnAlert bool `json:"autoSwapOnAlert"`
}

// defaultThresholdPct is used when no threshold has been configured.
const defaultThresholdPct = 90

// usageResponse is the subset of the /usage payload we render.
type usageResponse struct {
	FiveHour window `json:"five_hour"`
	SevenDay window `json:"seven_day"`
}

type window struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

func configPath() string {
	dir := filepath.Join(os.Getenv("HOME"), ".config", "claude-usage")
	return filepath.Join(dir, "accounts.json")
}

func loadStore() (*store, error) {
	b, err := os.ReadFile(configPath())
	if os.IsNotExist(err) {
		return &store{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s store
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if s.Settings.NotifyThresholdPct <= 0 {
		s.Settings.NotifyThresholdPct = defaultThresholdPct
	}
	return &s, nil
}

func saveStore(s *store) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// normalizeStore backfills missing email/uuid identities (for accounts saved
// before identity tracking) and removes duplicate accounts that resolve to the
// same uuid, keeping the most recently added. It persists any change.
func normalizeStore(s *store) {
	changed := false
	for i := range s.Accounts {
		if s.Accounts[i].AccountUUID == "" || s.Accounts[i].Email == "" {
			if uuid, email, err := fetchProfile(s.Accounts[i].Credential.AccessToken); err == nil {
				s.Accounts[i].AccountUUID = uuid
				s.Accounts[i].Email = email
				changed = true
			}
		}
	}
	// Dedup by uuid, keeping the last occurrence (most recently added/updated).
	seen := map[string]int{}
	out := make([]account, 0, len(s.Accounts))
	for _, a := range s.Accounts {
		if a.AccountUUID == "" {
			out = append(out, a) // unresolved; keep as-is
			continue
		}
		if idx, ok := seen[a.AccountUUID]; ok {
			out[idx] = a // replace earlier duplicate
			changed = true
			continue
		}
		seen[a.AccountUUID] = len(out)
		out = append(out, a)
	}
	s.Accounts = out
	if changed {
		_ = saveStore(s)
	}
}

// keychainCredential reads the current Claude Code credential from the macOS Keychain.
func keychainCredential() (credential, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", keychainName, "-w").Output()
	if err != nil {
		return credential{}, fmt.Errorf("read keychain %q: %w", keychainName, err)
	}
	var wrapper struct {
		ClaudeAiOauth *credential `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(out, &wrapper); err == nil && wrapper.ClaudeAiOauth != nil {
		return *wrapper.ClaudeAiOauth, nil
	}
	var c credential
	if err := json.Unmarshal(out, &c); err != nil {
		return credential{}, fmt.Errorf("parse keychain credential: %w", err)
	}
	return c, nil
}

// keychainAcctAttr returns the "account" attribute of the Claude Code keychain
// item, needed to update it in place. The attribute is printed to the combined
// output of `security ... -g` as: "acct"<blob>="<value>".
func keychainAcctAttr() (string, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", keychainName, "-g").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read keychain attrs %q: %w", keychainName, err)
	}
	m := acctAttrRe.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("keychain item %q has no acct attribute", keychainName)
	}
	return string(m[1]), nil
}

var acctAttrRe = regexp.MustCompile(`"acct"<blob>="([^"]*)"`)

// writeKeychainCredential replaces the Claude Code keychain credential in place,
// preserving the item's ACL so Claude Code keeps reading it without a re-auth
// prompt. It writes Claude Code's native {"claudeAiOauth": {...}} shape. The
// running Claude Code session picks this up on its next request.
func writeKeychainCredential(c credential) error {
	acct, err := keychainAcctAttr()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		ClaudeAiOauth credential `json:"claudeAiOauth"`
	}{c})
	if err != nil {
		return err
	}
	// -U updates the existing item (matched by acct+service), preserving its ACL.
	out, err := exec.Command("security", "add-generic-password",
		"-U", "-a", acct, "-s", keychainName, "-w", string(payload)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("write keychain %q: %w: %s", keychainName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// refresh exchanges the refresh token for a fresh access token, returning the
// updated credential.
func refresh(c credential) (credential, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": c.RefreshToken,
		"client_id":     oauthClientID,
	})
	req, err := http.NewRequest(http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return c, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return c, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return c, fmt.Errorf("refresh failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return c, fmt.Errorf("parse refresh response: %w", err)
	}
	c.AccessToken = r.AccessToken
	if r.RefreshToken != "" {
		c.RefreshToken = r.RefreshToken
	}
	c.ExpiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).UnixMilli()
	return c, nil
}

// fetchUsage returns usage for an account, refreshing its token if needed.
// The (possibly) updated credential is returned so the caller can persist it.
func fetchUsage(c credential) (usageResponse, credential, error) {
	if c.ExpiresAt > 0 && time.Now().UnixMilli() > c.ExpiresAt-60_000 {
		nc, err := refresh(c)
		if err != nil {
			return usageResponse{}, c, err
		}
		c = nc
	}
	u, err := callUsage(c.AccessToken)
	if err == errUnauthorized {
		nc, rerr := refresh(c)
		if rerr != nil {
			return usageResponse{}, c, rerr
		}
		c = nc
		u, err = callUsage(c.AccessToken)
	}
	return u, c, err
}

var errUnauthorized = fmt.Errorf("unauthorized")

// fetchProfile returns the stable account uuid and email for a token.
func fetchProfile(token string) (uuid, email string, err error) {
	req, err := http.NewRequest(http.MethodGet, profileURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBeta)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return "", "", errUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("profile error (%d)", resp.StatusCode)
	}
	var p struct {
		Account struct {
			UUID  string `json:"uuid"`
			Email string `json:"email"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", err
	}
	return p.Account.UUID, p.Account.Email, nil
}

func callUsage(token string) (usageResponse, error) {
	req, err := http.NewRequest(http.MethodGet, usageURL, nil)
	if err != nil {
		return usageResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return usageResponse{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		var u usageResponse
		if err := json.Unmarshal(raw, &u); err != nil {
			return usageResponse{}, fmt.Errorf("parse usage: %w", err)
		}
		return u, nil
	case http.StatusUnauthorized:
		return usageResponse{}, errUnauthorized
	case http.StatusTooManyRequests:
		return usageResponse{}, fmt.Errorf("rate limited (429) — polling too often")
	default:
		return usageResponse{}, fmt.Errorf("usage error (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

// runningInBundle reports whether this binary was launched from inside a macOS
// .app bundle (double-clicked or opened via a LaunchAgent), in which case there
// are no CLI args and we default to the menu bar app.
func runningInBundle() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.Contains(exe, ".app/Contents/MacOS/")
}

func main() {
	if len(os.Args) == 1 && runningInBundle() {
		cmdMenubar()
		return
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "login":
			cmdLogin()
			return
		case "add":
			cmdAdd()
			return
		case "list":
			cmdList()
			return
		case "rm":
			cmdRm(os.Args[2:])
			return
		case "menubar":
			cmdMenubar()
			return
		case "-h", "--help", "help":
			usage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
			usage()
			os.Exit(2)
		}
	}
	cmdDashboard()
}

func usage() {
	fmt.Print(`claude-usage — monitor Claude Max usage across accounts

  claude-usage login          browser OAuth for an account (no Claude Code session change)
  claude-usage add            capture the account currently logged into Claude Code
  claude-usage list           list configured accounts
  claude-usage rm <email>     remove an account
  claude-usage menubar        run the macOS menu bar app (👾 NN%)
  claude-usage                live dashboard

Accounts are identified by their email (read from the account profile) — no
manual naming. Add extra accounts without disrupting Claude Code: run 'login'
and sign in to that account in the browser. Nothing touches Claude Code's session.
`)
}

// cmdLogin runs a standalone OAuth PKCE login for one account. It opens the
// browser, the user signs in to the desired account, pastes back the code, and
// the resulting tokens are stored, identified by the account email — independent
// of Claude Code.
func cmdLogin() {
	verifier := randB64URL(32)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randB64URL(32)

	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", oauthClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", oauthScopes)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	authURL := authorizeURL + "?" + q.Encode()

	fmt.Println("Opening browser to sign in. Sign in to the account you want to add,")
	fmt.Println("then copy the authorization code shown and paste it here.")
	fmt.Println()
	fmt.Println(authURL)
	fmt.Println()
	_ = exec.Command("open", authURL).Start()

	fmt.Print("Paste code: ")
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		fmt.Fprintln(os.Stderr, "no input")
		os.Exit(1)
	}
	pasted := strings.TrimSpace(sc.Text())
	code := pasted
	if c, s, ok := strings.Cut(pasted, "#"); ok { // format: <code>#<state>
		code = c
		if s != state {
			fmt.Fprintln(os.Stderr, "error: OAuth state mismatch — possible CSRF or stale code; retry login")
			os.Exit(1)
		}
	}

	c, err := exchangeCode(code, state, verifier)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	email, err := upsertAccount(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("logged in and saved account %s\n", email)
}

// exchangeCode swaps an authorization code for tokens.
func exchangeCode(code, state, verifier string) (credential, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"state":         state,
		"client_id":     oauthClientID,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	})
	req, err := http.NewRequest(http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return credential{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return credential{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return credential{}, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return credential{}, fmt.Errorf("parse token response: %w", err)
	}
	c := credential{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).UnixMilli(),
		Scopes:       strings.Fields(r.Scope),
	}
	return c, nil
}

func randB64URL(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// upsertAccount resolves the credential's stable identity (email/uuid) via the
// profile endpoint, then adds or replaces the matching account and persists the
// store. It returns the account email.
func upsertAccount(c credential) (string, error) {
	uuid, email, err := fetchProfile(c.AccessToken)
	if err != nil {
		return "", fmt.Errorf("resolve account identity: %w", err)
	}
	s, err := loadStore()
	if err != nil {
		return "", err
	}
	replaced := false
	for i := range s.Accounts {
		if s.Accounts[i].AccountUUID == uuid {
			s.Accounts[i].Email = email
			s.Accounts[i].Credential = c
			replaced = true
		}
	}
	if !replaced {
		s.Accounts = append(s.Accounts, account{Email: email, AccountUUID: uuid, Credential: c})
	}
	return email, saveStore(s)
}

func cmdAdd() {
	c, err := keychainCredential()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	email, err := upsertAccount(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("saved account %s\n", email)
}

func cmdList() {
	s, err := loadStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if len(s.Accounts) == 0 {
		fmt.Println("no accounts configured — run: claude-usage login")
		return
	}
	normalizeStore(s)
	for _, a := range s.Accounts {
		fmt.Println(a.Email)
	}
}

func cmdRm(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-usage rm <email>")
		os.Exit(2)
	}
	s, err := loadStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	out := s.Accounts[:0]
	for _, a := range s.Accounts {
		if a.Email != args[0] {
			out = append(out, a)
		}
	}
	if len(out) == len(s.Accounts) {
		fmt.Fprintf(os.Stderr, "error: no account with email %q\n", args[0])
		os.Exit(1)
	}
	s.Accounts = out
	if err := saveStore(s); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("removed %s\n", args[0])
}

// result holds a rendered snapshot for one account.
type result struct {
	email  string
	uuid   string // stable account uuid, for mapping back to the stored credential
	usage  usageResponse
	active bool // this account is the one currently logged into Claude Code
	err    error
}

// activeUUID returns the account uuid currently logged into Claude Code, or ""
// if it can't be determined (no keychain entry, offline, etc.). Best-effort.
func activeUUID() string {
	c, err := keychainCredential()
	if err != nil {
		return ""
	}
	uuid, _, err := fetchProfile(c.AccessToken)
	if err != nil {
		return ""
	}
	return uuid
}

func poll(s *store) []result {
	active := activeUUID()
	results := make([]result, len(s.Accounts))
	changed := false
	for i := range s.Accounts {
		u, nc, err := fetchUsage(s.Accounts[i].Credential)
		if nc.AccessToken != s.Accounts[i].Credential.AccessToken {
			s.Accounts[i].Credential = nc
			changed = true
		}
		results[i] = result{
			email:  s.Accounts[i].Email,
			uuid:   s.Accounts[i].AccountUUID,
			usage:  u,
			active: active != "" && s.Accounts[i].AccountUUID == active,
			err:    err,
		}
	}
	if changed {
		_ = saveStore(s)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].email < results[j].email })
	return results
}

// --- Bubble Tea TUI ---

type pollMsg []result

type model struct {
	store   *store
	results []result
	updated time.Time
	loading bool
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.pollCmd(), tea.EnterAltScreen)
}

func (m model) pollCmd() tea.Cmd {
	s := m.store
	return func() tea.Msg { return pollMsg(poll(s)) }
}

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

type tickMsg struct{}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			if !m.loading {
				m.loading = true
				return m, m.pollCmd()
			}
		}
	case pollMsg:
		m.results = msg
		m.updated = time.Now()
		m.loading = false
		return m, tick()
	case tickMsg:
		m.loading = true
		return m, m.pollCmd()
	}
	return m, nil
}

var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	styleActive = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	styleEmail  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

func (m model) View() string {
	now := time.Now()
	var b strings.Builder
	status := ""
	if m.loading {
		status = "  ⟳ refreshing…"
	}
	b.WriteString(styleTitle.Render("Claude Max Usage") +
		styleDim.Render("   "+m.updated.Format("15:04:05")+status) + "\n")
	b.WriteString(styleDim.Render("r refresh · q quit · ● = active Claude Code session") + "\n\n")

	for _, r := range m.results {
		marker := "  "
		name := styleEmail.Render(r.email)
		if r.active {
			marker = styleActive.Render("● ")
			name = styleActive.Render(r.email + "  (active)")
		}
		b.WriteString(marker + name + "\n")
		if r.err != nil {
			b.WriteString("    " + styleErr.Render(r.err.Error()) + "\n\n")
			continue
		}
		b.WriteString("    " + styleDim.Render("5-hour ") + bar(r.usage.FiveHour, now) + "\n")
		b.WriteString("    " + styleDim.Render("7-day  ") + bar(r.usage.SevenDay, now) + "\n\n")
	}
	return b.String()
}

func cmdDashboard() {
	s, err := loadStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if len(s.Accounts) == 0 {
		fmt.Println("no accounts configured — run: claude-usage login")
		return
	}
	normalizeStore(s)
	if _, err := tea.NewProgram(model{store: s, loading: true}).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// bar renders a colored utilization bar with percentage and reset countdown.
func bar(w window, now time.Time) string {
	const width = 24
	filled := min(max(int(w.Utilization/100*width+0.5), 0), width)
	color := lipgloss.Color("10") // green
	switch {
	case w.Utilization >= 90:
		color = lipgloss.Color("9") // red
	case w.Utilization >= 70:
		color = lipgloss.Color("11") // yellow
	}
	fill := lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("█", filled))
	rest := styleDim.Render(strings.Repeat("░", width-filled))
	reset := ""
	if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
		if d := t.Sub(now); d > 0 {
			reset = styleDim.Render("  resets in " + shortDur(d))
		}
	}
	return fmt.Sprintf("%s%s %5.1f%%%s", fill, rest, w.Utilization, reset)
}

func shortDur(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h >= 24 {
		return fmt.Sprintf("%dd %dh", h/24, h%24)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
