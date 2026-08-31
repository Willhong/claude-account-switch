package app

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Willhong/claude-account-switch/internal/claudeauth"
	"github.com/Willhong/claude-account-switch/internal/store"
	"github.com/Willhong/claude-account-switch/internal/target"
	"github.com/Willhong/claude-account-switch/internal/ui"
)

// loginTimeout bounds how long cas waits for the browser round-trip.
const loginTimeout = 5 * time.Minute

// CmdLogin registers a new account by running the OAuth flow itself, so the
// account Claude Code is currently signed in as is never disturbed.
func CmdLogin(args []string) error {
	fs := flag.NewFlagSet("cas login", flag.ContinueOnError)
	label := fs.String("label", "", "short name for the slot (e.g. work, personal)")
	manual := fs.Bool("manual", false, "print the URL and paste the code back, instead of using a local callback server")
	port := fs.Int("port", 0, "local callback port (default: an unused one)")
	hint := fs.String("email", "", "pre-fill this address on the sign-in page")
	activate := fs.Bool("activate", false, "switch Claude Code to the new account once it is registered")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}

	a, err := New()
	if err != nil {
		return err
	}
	defer a.Close()

	a.SyncActive()

	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	defer cancel()

	pkce, err := claudeauth.NewPKCE()
	if err != nil {
		return err
	}

	var code, redirectURI string
	if *manual {
		redirectURI = claudeauth.ManualRedirectURI
		code, err = manualLogin(a.OAuth, pkce, redirectURI, *hint)
	} else {
		code, redirectURI, err = browserLogin(ctx, a.OAuth, pkce, *port, *hint)
	}
	if err != nil {
		return err
	}

	ui.Infof("Exchanging the authorization code…")
	res, err := a.OAuth.Exchange(ctx, code, pkce, redirectURI)
	if err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}

	profile, perr := a.OAuth.FetchProfile(ctx, res.Cred.AccessToken)
	if perr != nil {
		ui.Warnf("signed in, but the account profile could not be read: %v", perr)
	}
	var roles *claudeauth.Roles
	if perr == nil {
		roles, _ = a.OAuth.FetchRoles(ctx, res.Cred.AccessToken)
	}

	// Re-logging into an account cas already knows updates that slot instead
	// of piling up duplicates.
	slot := a.State.FindByAccountUUID(res.AccountUUID)
	if slot == nil && profile != nil {
		slot = a.State.FindByAccountUUID(profile.Account.UUID)
	}
	if slot == nil {
		slot = a.State.FindByEmail(res.Email)
	}
	isNew := slot == nil
	if isNew {
		slot = a.State.Add(&store.Slot{
			Label:       *label,
			Email:       res.Email,
			AccountUUID: res.AccountUUID,
			OrgUUID:     res.OrgUUID,
		})
	} else if *label != "" {
		slot.Label = *label
	}
	slot.Revoked = false
	slot.LastError = ""

	env := claudeauth.NewEnvelope(res.Cred)
	if profile != nil {
		applyProfile(slot, env.OAuth, profile, roles)
	}
	if err := a.Store.WriteCred(slot.N, slot.Label, env); err != nil {
		if isNew {
			a.State.Remove(slot.N)
		}
		return err
	}
	slot.ApplyCred(env.OAuth)
	slot.LastRefreshedAt = time.Now()

	if err := a.Save(); err != nil {
		return err
	}

	verb := "Registered"
	if !isNew {
		verb = "Refreshed the login for"
	}
	ui.OKf("%s %s as slot %d.", verb, ui.Bold(slot.Email), slot.N)

	// The very first account is switched in automatically; otherwise the
	// current session is left alone unless --activate was given.
	shouldActivate := *activate || a.State.ActiveSlot == 0
	if shouldActivate {
		if err := a.activate(ctx, slot); err != nil {
			return err
		}
		if err := a.Save(); err != nil {
			return err
		}
	} else {
		ui.Infof("Run `cas switch %d` to use it.", slot.N)
	}
	return nil
}

// browserLogin opens the consent page and waits for the local callback.
func browserLogin(ctx context.Context, cfg claudeauth.Config, pkce *claudeauth.PKCE, port int, hint string) (code, redirectURI string, err error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", "", fmt.Errorf("start the local callback server: %w", err)
	}
	defer ln.Close()

	actual := ln.Addr().(*net.TCPAddr).Port
	redirectURI = fmt.Sprintf("http://localhost:%d/callback", actual)
	authURL := cfg.AuthorizeURLFor(pkce, redirectURI, hint)

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			detail := q.Get("error_description")
			writeCallbackPage(w, false, "Sign-in failed", firstNonEmpty(detail, e))
			results <- result{err: fmt.Errorf("authorization failed: %s", firstNonEmpty(detail, e))}
			return
		}
		if got := q.Get("state"); got != "" && got != pkce.State {
			writeCallbackPage(w, false, "Sign-in failed", "The response did not match this sign-in attempt.")
			results <- result{err: errors.New("state mismatch: the callback does not belong to this login attempt")}
			return
		}
		c := q.Get("code")
		if c == "" {
			writeCallbackPage(w, false, "Sign-in failed", "No authorization code was returned.")
			results <- result{err: errors.New("callback carried no authorization code")}
			return
		}
		writeCallbackPage(w, true, "Signed in", "You can close this tab and return to the terminal.")
		results <- result{code: c}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	ui.Infof("Opening your browser to sign in to Claude…")
	if err := openBrowser(authURL); err != nil {
		ui.Warnf("could not open a browser (%v)", err)
	}
	fmt.Fprintf(os.Stderr, "\nIf it did not open, visit:\n  %s\n\n", authURL)
	ui.Infof("Waiting for the callback on %s …", redirectURI)

	select {
	case r := <-results:
		if r.err != nil {
			return "", "", r.err
		}
		return r.code, redirectURI, nil
	case <-ctx.Done():
		return "", "", fmt.Errorf("timed out after %s waiting for the browser; retry with `cas login --manual`", loginTimeout)
	}
}

// manualLogin is the fallback for machines with no browser, or when the local
// redirect is blocked. The user pastes the code Anthropic displays.
func manualLogin(cfg claudeauth.Config, pkce *claudeauth.PKCE, redirectURI, hint string) (string, error) {
	authURL := cfg.AuthorizeURLFor(pkce, redirectURI, hint)
	fmt.Fprintf(os.Stderr, "Open this URL, sign in, then paste the code shown:\n\n  %s\n\n", authURL)
	fmt.Fprint(os.Stderr, "Code: ")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("read the authorization code: %w", err)
	}
	code := strings.TrimSpace(line)
	if code == "" {
		return "", errors.New("no authorization code given")
	}
	return code, nil
}

// CmdAdopt registers the account Claude Code is already signed in as.
func CmdAdopt(args []string) error {
	fs := flag.NewFlagSet("cas adopt", flag.ContinueOnError)
	label := fs.String("label", "", "short name for the slot (e.g. work, personal)")
	if err := parseNoArgs(fs, args); err != nil {
		return err
	}

	a, err := New()
	if err != nil {
		return err
	}
	defer a.Close()

	env, err := a.Target.ReadCred()
	if err != nil {
		if errors.Is(err, target.ErrNoCredential) {
			return errors.New("Claude Code has no credential stored — run `cas login` instead")
		}
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Rotate first if the live token is stale, so the adopted slot starts warm.
	if env.OAuth.AccessExpired(switchSkew) && !env.OAuth.RefreshExpired() {
		if res, rerr := a.OAuth.Refresh(ctx, env.OAuth); rerr == nil {
			next := env.Clone()
			next.OAuth = res.Cred
			env = next
			if werr := a.Target.WriteCred(env); werr != nil {
				ui.Warnf("could not write the refreshed token back to Claude Code: %v", werr)
			}
		}
	}

	rawAccount, _ := a.Target.ReadOAuthAccount()
	slot := a.State.FindByAccountUUID(target.AccountUUID(rawAccount))

	profile, perr := a.OAuth.FetchProfile(ctx, env.OAuth.AccessToken)
	if perr != nil {
		ui.Warnf("could not read the account profile: %v", perr)
	}
	var roles *claudeauth.Roles
	if perr == nil {
		roles, _ = a.OAuth.FetchRoles(ctx, env.OAuth.AccessToken)
		if slot == nil {
			slot = a.State.FindByAccountUUID(profile.Account.UUID)
		}
	}

	if slot != nil {
		ui.Infof("That account is already registered as slot %d.", slot.N)
	} else {
		slot = a.State.Add(&store.Slot{
			Label:        *label,
			Email:        target.AccountEmail(rawAccount),
			AccountUUID:  target.AccountUUID(rawAccount),
			OAuthAccount: rawAccount,
		})
	}
	if *label != "" {
		slot.Label = *label
	}
	if profile != nil {
		applyProfile(slot, env.OAuth, profile, roles)
	} else if len(rawAccount) > 0 && len(slot.OAuthAccount) == 0 {
		slot.OAuthAccount = rawAccount
	}
	if slot.Email == "" {
		slot.Email = "(unknown)"
	}

	if err := a.Store.WriteCred(slot.N, slot.Label, env); err != nil {
		return err
	}
	slot.ApplyCred(env.OAuth)
	slot.Revoked = false
	slot.LastError = ""
	a.State.ActiveSlot = slot.N

	if err := a.Save(); err != nil {
		return err
	}
	ui.OKf("Registered %s as slot %d (currently active).", ui.Bold(slot.Email), slot.N)
	return nil
}

func openBrowser(url string) error {
	return exec.Command("/usr/bin/open", url).Start()
}

func writeCallbackPage(w http.ResponseWriter, ok bool, heading, message string) {
	status := "Signed in"
	accent := "#1a7f37"
	if !ok {
		status = "Error"
		accent = "#b42318"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>cas</title>
<style>body{font-family:ui-sans-serif,-apple-system,system-ui,sans-serif;margin:0;
display:grid;place-items:center;min-height:100vh;background:#faf9f7;color:#1c1b19}
main{text-align:center;padding:40px 24px;max-width:34rem}
.status{display:inline-block;font-size:12px;letter-spacing:.08em;text-transform:uppercase;
color:%s;border:1px solid currentColor;border-radius:999px;padding:4px 12px;margin-bottom:20px}
h1{font-size:26px;margin:0 0 10px}p{margin:0;color:#5c5955;line-height:1.6}
@media(prefers-color-scheme:dark){body{background:#141413;color:#eeece7}p{color:#a3a09b}}
</style></head><body><main><span class="status">%s</span><h1>%s</h1><p>%s</p></main>%s</body></html>`,
		accent, status, html.EscapeString(heading), html.EscapeString(message),
		map[bool]string{true: `<script>setTimeout(function(){try{window.close()}catch(e){}},1500)</script>`}[ok])
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
