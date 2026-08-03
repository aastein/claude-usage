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
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// oauthClientID is Claude Code's public OAuth client id, used for the login and
// token-refresh flows (PKCE, no client secret).
const oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

const (
	usageURL     = "https://api.anthropic.com/api/oauth/usage"
	authorizeURL = "https://claude.ai/oauth/authorize"
	tokenURL     = "https://platform.claude.com/v1/oauth/token"
	redirectURI  = "https://platform.claude.com/oauth/code/callback"
	oauthScopes  = "user:profile user:inference user:sessions:claude_code user:mcp_servers"
	oauthBeta    = "oauth-2025-04-20"
	keychainName = "Claude Code-credentials"
	pollInterval = 5 * time.Minute
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

// account pairs a user-facing label with its credential.
type account struct {
	Label      string     `json:"label"`
	Credential credential `json:"credential"`
}

// store is the on-disk config: the set of monitored accounts.
type store struct {
	Accounts []account `json:"accounts"`
}

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

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "login":
			cmdLogin(os.Args[2:])
			return
		case "add":
			cmdAdd(os.Args[2:])
			return
		case "list":
			cmdList()
			return
		case "rename", "mv":
			cmdRename(os.Args[2:])
			return
		case "rm":
			cmdRm(os.Args[2:])
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

  claude-usage login <label>  browser OAuth for an account (no Claude Code session change)
  claude-usage add <label>    capture the account currently logged into Claude Code
  claude-usage list           list configured accounts
  claude-usage rename <old> <new>  rename an account
  claude-usage rm <label>     remove an account
  claude-usage                live dashboard

Add extra accounts without disrupting Claude Code: run 'login <label>' and sign
in to that account in the browser. Nothing touches Claude Code's session.
`)
}

// cmdLogin runs a standalone OAuth PKCE login for one account. It opens the
// browser, the user signs in to the desired account, pastes back the code, and
// the resulting tokens are stored under the label — independent of Claude Code.
func cmdLogin(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-usage login <label>")
		os.Exit(2)
	}
	label := args[0]

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
	if err := upsertAccount(label, c); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("logged in and saved account %q (subscription: %s)\n", label, c.SubscriptionType)
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

// upsertAccount adds or replaces an account by label and persists the store.
func upsertAccount(label string, c credential) error {
	s, err := loadStore()
	if err != nil {
		return err
	}
	replaced := false
	for i := range s.Accounts {
		if s.Accounts[i].Label == label {
			s.Accounts[i].Credential = c
			replaced = true
		}
	}
	if !replaced {
		s.Accounts = append(s.Accounts, account{Label: label, Credential: c})
	}
	return saveStore(s)
}

func cmdAdd(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-usage add <label>")
		os.Exit(2)
	}
	label := args[0]
	c, err := keychainCredential()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err := upsertAccount(label, c); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("saved account %q (subscription: %s)\n", label, c.SubscriptionType)
}

func cmdList() {
	s, err := loadStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if len(s.Accounts) == 0 {
		fmt.Println("no accounts configured — run: claude-usage add <label>")
		return
	}
	for _, a := range s.Accounts {
		fmt.Printf("%-16s %s\n", a.Label, a.Credential.SubscriptionType)
	}
}

func cmdRename(args []string) {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: claude-usage rename <old> <new>")
		os.Exit(2)
	}
	oldLabel, newLabel := args[0], args[1]
	s, err := loadStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	for _, a := range s.Accounts {
		if a.Label == newLabel {
			fmt.Fprintf(os.Stderr, "error: an account named %q already exists\n", newLabel)
			os.Exit(1)
		}
	}
	found := false
	for i := range s.Accounts {
		if s.Accounts[i].Label == oldLabel {
			s.Accounts[i].Label = newLabel
			found = true
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "error: no account named %q\n", oldLabel)
		os.Exit(1)
	}
	if err := saveStore(s); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("renamed %q to %q\n", oldLabel, newLabel)
}

func cmdRm(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-usage rm <label>")
		os.Exit(2)
	}
	s, err := loadStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	out := s.Accounts[:0]
	for _, a := range s.Accounts {
		if a.Label != args[0] {
			out = append(out, a)
		}
	}
	if len(out) == len(s.Accounts) {
		fmt.Fprintf(os.Stderr, "error: no account named %q\n", args[0])
		os.Exit(1)
	}
	s.Accounts = out
	if err := saveStore(s); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("removed %q\n", args[0])
}

// result holds a rendered snapshot for one account.
type result struct {
	label string
	usage usageResponse
	err   error
}

func cmdDashboard() {
	s, err := loadStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if len(s.Accounts) == 0 {
		fmt.Println("no accounts configured — run: claude-usage add <label>")
		return
	}

	// Restore the cursor on Ctrl-C / SIGTERM, not just on 'q'.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Print("\033[?25h")
		os.Exit(0)
	}()

	// 'q' + Enter to quit, 'r' + Enter to force refresh.
	refreshCh := make(chan struct{}, 1)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			switch strings.TrimSpace(sc.Text()) {
			case "q":
				fmt.Print("\033[?25h")
				os.Exit(0)
			case "r":
				select {
				case refreshCh <- struct{}{}:
				default:
				}
			}
		}
		_ = sc.Err() // stdin closed; keyboard controls stop but the dashboard keeps polling
	}()

	fmt.Print("\033[?25l") // hide cursor
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		results := poll(s)
		render(results)
		select {
		case <-ticker.C:
		case <-refreshCh:
		}
	}
}

func poll(s *store) []result {
	results := make([]result, len(s.Accounts))
	changed := false
	for i := range s.Accounts {
		u, nc, err := fetchUsage(s.Accounts[i].Credential)
		if nc.AccessToken != s.Accounts[i].Credential.AccessToken {
			s.Accounts[i].Credential = nc
			changed = true
		}
		results[i] = result{label: s.Accounts[i].Label, usage: u, err: err}
	}
	if changed {
		_ = saveStore(s)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].label < results[j].label })
	return results
}

func render(results []result) {
	fmt.Print("\033[H\033[2J") // home + clear
	now := time.Now()
	fmt.Printf("  Claude Max Usage — %s   (r+Enter refresh · q+Enter quit)\n\n", now.Format("15:04:05"))
	labelW := 8
	for _, r := range results {
		if len(r.label) > labelW {
			labelW = len(r.label)
		}
	}
	for _, r := range results {
		if r.err != nil {
			fmt.Printf("  %-*s  \033[31m%s\033[0m\n\n", labelW, r.label, r.err)
			continue
		}
		fmt.Printf("  %-*s\n", labelW, r.label)
		fmt.Printf("    5-hour  %s\n", bar(r.usage.FiveHour, now))
		fmt.Printf("    7-day   %s\n\n", bar(r.usage.SevenDay, now))
	}
}

// bar renders a colored utilization bar with percentage and reset countdown.
func bar(w window, now time.Time) string {
	const width = 24
	filled := min(max(int(w.Utilization/100*width+0.5), 0), width)
	color := "\033[32m" // green
	switch {
	case w.Utilization >= 90:
		color = "\033[31m" // red
	case w.Utilization >= 70:
		color = "\033[33m" // yellow
	}
	b := color + strings.Repeat("█", filled) + "\033[90m" + strings.Repeat("░", width-filled) + "\033[0m"
	reset := ""
	if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
		d := t.Sub(now)
		if d > 0 {
			reset = fmt.Sprintf("  resets in %s", shortDur(d))
		}
	}
	return fmt.Sprintf("%s %5.1f%%%s", b, w.Utilization, reset)
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
