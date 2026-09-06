package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/erickardus/ai-gateway/internal/ccsettings"
	"github.com/erickardus/ai-gateway/internal/sso"
)

// loginTimeout bounds how long the CLI waits for a browser round trip before
// giving the terminal back.
const loginTimeout = 5 * time.Minute

// grant is what /sso/exchange and /sso/renew return.
type grant struct {
	Identity     string            `json:"identity"`
	Subject      string            `json:"subject"`
	Role         string            `json:"role"`
	ExpiresAt    time.Time         `json:"expires_at"`
	RenewWithin  string            `json:"renew_within"`
	RefreshToken string            `json:"refresh_token"`
	Env          map[string]string `json:"env"`
}

// loginOptions is what both the interactive and the renewal paths need.
type loginOptions struct {
	gateway    string
	device     string
	configDir  string
	renew      bool
	ifExpiring bool
	noHook     bool
	timeout    time.Duration
	// deviceSet records whether -device was given, so a renewal can keep the
	// name the login used rather than silently issuing a second key under this
	// machine's current hostname.
	deviceSet bool
}

// runLogin signs a developer in and configures Claude Code for them.
func runLogin(args []string) error {
	fs := flag.NewFlagSet("gateway login", flag.ContinueOnError)
	var o loginOptions
	fs.StringVar(&o.gateway, "gateway", "", "base URL of the gateway to sign in to (remembered after the first login)")
	fs.StringVar(&o.device, "device", defaultDevice(), "name for this machine, so a second one gets its own key")
	fs.StringVar(&o.configDir, "config-dir", "", "Claude Code configuration directory (default $CLAUDE_CONFIG_DIR, else ~/.claude)")
	fs.BoolVar(&o.renew, "renew", false, "renew without a browser, using the refresh token from the last login")
	fs.BoolVar(&o.ifExpiring, "if-expiring", false, "with -renew, do nothing unless the key is close to expiring")
	fs.BoolVar(&o.noHook, "no-hook", false, "do not install the SessionStart hook that renews the key")
	fs.DurationVar(&o.timeout, "timeout", loginTimeout, "how long to wait for the browser")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: gateway login [flags]\n\n"+
			"Signs in through your organisation's identity provider and writes the\n"+
			"resulting key into Claude Code's settings. The key travels in a custom\n"+
			"header, so your claude.ai subscription login keeps working.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "device" {
			o.deviceSet = true
		}
	})

	dir := o.configDir
	if dir == "" {
		var err error
		if dir, err = ccsettings.Dir(); err != nil {
			return err
		}
	}
	statePath := ccsettings.StatePath(dir)
	state, haveState, err := ccsettings.LoadState(statePath)
	if err != nil {
		return err
	}

	if o.gateway == "" && haveState {
		o.gateway = state.Gateway
	}
	if o.gateway == "" {
		return errors.New("no gateway address: pass -gateway https://your-gateway.example.com")
	}
	o.gateway = strings.TrimSuffix(o.gateway, "/")
	if _, err := url.Parse(o.gateway); err != nil {
		return fmt.Errorf("-gateway is not a valid URL: %w", err)
	}

	if o.renew {
		return renewLogin(o, dir, statePath, state, haveState)
	}
	return interactiveLogin(o, dir, statePath)
}

// interactiveLogin runs the browser flow.
func interactiveLogin(o loginOptions, dir, statePath string) error {
	settingsPath := ccsettings.SettingsPath(dir)
	// Checked before the browser opens, so a developer is not sent through a
	// sign-in only to be told at the end that the result cannot be used.
	if err := checkConflicts(settingsPath); err != nil {
		return err
	}

	verifier, err := sso.NewToken()
	if err != nil {
		return err
	}

	// Bound to loopback explicitly. The listener receives a one-time code that
	// can be redeemed for a key, so it must not be reachable from the network.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("open a local listener for the sign-in: %w", err)
	}
	defer ln.Close()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			if e := q.Get("error"); e != "" {
				browserPage(w, http.StatusBadRequest, "Sign-in failed", e)
				results <- result{err: errors.New(e)}
				return
			}
			code := q.Get("code")
			if code == "" {
				browserPage(w, http.StatusBadRequest, "Sign-in failed", "the gateway returned no code")
				results <- result{err: errors.New("the gateway returned no code")}
				return
			}
			browserPage(w, http.StatusOK, "You're signed in", "Claude Code has been configured. You can close this tab.")
			results <- result{code: code}
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	loginURL := o.gateway + "/sso/login?" + url.Values{
		"redirect_uri":          {redirectURI},
		"code_challenge":        {sso.Challenge(verifier)},
		"code_challenge_method": {"S256"},
		"device":                {o.device},
	}.Encode()

	fmt.Fprintln(os.Stderr, "Opening your browser to sign in.")
	fmt.Fprintf(os.Stderr, "If it does not open, visit:\n\n  %s\n\n", loginURL)
	openBrowser(loginURL)

	// Ctrl-C should return the terminal rather than leave a listener behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var code string
	select {
	case res := <-results:
		if res.err != nil {
			return fmt.Errorf("sign-in failed: %s", res.err)
		}
		code = res.code
	case <-time.After(o.timeout):
		return fmt.Errorf("timed out after %s waiting for the browser", o.timeout)
	case <-ctx.Done():
		return errors.New("cancelled")
	}

	g, err := postJSON[grant](context.Background(), o.gateway+"/sso/exchange", map[string]string{
		"code":          code,
		"code_verifier": verifier,
	})
	if err != nil {
		return err
	}
	return applyGrant(o, dir, statePath, g, true)
}

// renewLogin refreshes without a browser. It is what the SessionStart hook
// runs, so it is quiet when there is nothing to do.
func renewLogin(o loginOptions, dir, statePath string, state *ccsettings.State, haveState bool) error {
	if !haveState {
		return errors.New("not signed in: run \"gateway login\" first")
	}
	if state.RefreshToken == "" {
		return errors.New("the last sign-in returned no refresh token, so renewal is not possible: run \"gateway login\" again")
	}
	if o.ifExpiring && !state.RenewalDue(time.Now()) {
		return nil
	}
	// Renewing under a different device name would issue a second key and
	// retire nothing, so the stored name wins unless one was given explicitly.
	if !o.deviceSet && state.Device != "" {
		o.device = state.Device
	}

	g, err := postJSON[grant](context.Background(), o.gateway+"/sso/renew", map[string]string{
		"refresh_token": state.RefreshToken,
		"device":        o.device,
	})
	if err != nil {
		return err
	}
	return applyGrant(o, dir, statePath, g, false)
}

// applyGrant writes the key into Claude Code's settings and records the login.
func applyGrant(o loginOptions, dir, statePath string, g *grant, verbose bool) error {
	settingsPath := ccsettings.SettingsPath(dir)
	f, err := ccsettings.Load(settingsPath)
	if err != nil {
		return err
	}
	if conflicts := f.Conflicts(); len(conflicts) > 0 {
		return conflictError(settingsPath, conflicts)
	}

	changed := f.SetEnv(g.Env)
	if !o.noHook {
		if cmd, err := renewCommand(o); err == nil {
			changed = f.EnsureHook(cmd) || changed
		} else if verbose {
			fmt.Fprintf(os.Stderr, "warning: could not install the renewal hook: %v\n", err)
		}
	}
	if changed {
		if err := f.Save(); err != nil {
			return err
		}
	}

	if err := ccsettings.SaveState(statePath, &ccsettings.State{
		Gateway:      o.gateway,
		Identity:     g.Identity,
		Subject:      g.Subject,
		Device:       o.device,
		Role:         g.Role,
		ExpiresAt:    g.ExpiresAt,
		RenewWithin:  g.RenewWithin,
		RefreshToken: g.RefreshToken,
	}); err != nil {
		return err
	}

	if verbose {
		fmt.Printf("Signed in as %s", g.Identity)
		if g.Role != "" {
			fmt.Printf(" (role %s)", g.Role)
		}
		fmt.Printf("\nKey expires %s\n", g.ExpiresAt.Local().Format("2006-01-02 15:04"))
		if changed {
			fmt.Printf("Wrote %s\n", settingsPath)
		} else {
			fmt.Printf("%s was already up to date\n", settingsPath)
		}
		fmt.Println("\nStart Claude Code and run /login → \"Claude account with subscription\".")
	}
	return nil
}

// runLogout removes what a login wrote.
func runLogout(args []string) error {
	fs := flag.NewFlagSet("gateway logout", flag.ContinueOnError)
	configDir := fs.String("config-dir", "", "Claude Code configuration directory")
	keep := fs.Bool("keep-settings", false, "leave the Claude Code settings alone and only forget the refresh token")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir := *configDir
	if dir == "" {
		var err error
		if dir, err = ccsettings.Dir(); err != nil {
			return err
		}
	}

	if !*keep {
		f, err := ccsettings.Load(ccsettings.SettingsPath(dir))
		if err != nil {
			return err
		}
		changed := f.ClearEnv()
		if cmd, err := renewCommand(loginOptions{configDir: *configDir}); err == nil {
			changed = f.RemoveHook(cmd) || changed
		}
		if changed {
			if err := f.Save(); err != nil {
				return err
			}
		}
	}
	if err := ccsettings.DeleteState(ccsettings.StatePath(dir)); err != nil {
		return err
	}
	fmt.Println("Signed out. The key stays valid at the gateway until it expires or an operator revokes it.")
	return nil
}

// renewCommand is the hook command that keeps the key fresh.
//
// It resolves this binary's own path rather than relying on it being on PATH:
// a hook runs in whatever environment Claude Code was started from, which on a
// desktop launch is not the shell the developer installed the gateway into.
func renewCommand(o loginOptions) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	cmd := quoteArg(exe) + " login --renew --if-expiring"
	if o.configDir != "" {
		cmd += " --config-dir " + quoteArg(o.configDir)
	}
	return cmd, nil
}

// quoteArg quotes a path for the shell a hook runs under.
func quoteArg(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\$`") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// checkConflicts refuses to proceed when the settings already hold something
// that would displace the subscription.
func checkConflicts(path string) error {
	f, err := ccsettings.Load(path)
	if err != nil {
		return err
	}
	if conflicts := f.Conflicts(); len(conflicts) > 0 {
		return conflictError(path, conflicts)
	}
	return nil
}

func conflictError(path string, conflicts []string) error {
	return fmt.Errorf(
		"%s sets %s, which replaces your claude.ai subscription login with a per-token credential.\n"+
			"A gateway key written alongside it would work and quietly bill you per token.\n"+
			"Remove it from that file, then run this again",
		path, strings.Join(conflicts, " and "))
}

// postJSON sends a JSON request and decodes a JSON reply.
func postJSON[T any](ctx context.Context, endpoint string, body any) (*T, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the gateway at %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway refused the request: %s: %s", resp.Status, gatewayMessage(payload))
	}
	var out T
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("gateway returned an unreadable response: %w", err)
	}
	return &out, nil
}

// gatewayMessage pulls the human part out of the gateway's error envelope,
// falling back to the raw body when it is shaped differently.
func gatewayMessage(payload []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &env); err == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return strings.TrimSpace(string(payload))
}

// browserPage renders the tab the developer is left looking at.
func browserPage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title>`+
		`<body style="font:16px system-ui;margin:4rem auto;max-width:34rem;color:#111">`+
		`<h1 style="font-size:1.25rem">%s</h1><p>%s</p></body>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(message))
}

// openBrowser makes a best effort to open a URL. Failing is not an error: the
// URL was already printed for the developer to open themselves, which is also
// the path over SSH, where there is no browser to open.
//
// $BROWSER wins where it is set, following the convention xdg-open itself
// honours. It is what makes this path drivable from a test, and it is the
// escape hatch for a machine whose default handler is not the browser the
// developer is signed into.
func openBrowser(target string) {
	if custom := strings.Fields(os.Getenv("BROWSER")); len(custom) > 0 {
		_ = exec.Command(custom[0], append(custom[1:], target)...).Start()
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	_ = cmd.Start()
}

// defaultDevice names this machine.
func defaultDevice() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "unknown"
	}
	if i := strings.Index(host, "."); i > 0 {
		host = host[:i]
	}
	return host
}
