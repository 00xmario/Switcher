package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Account is one account as the command line reports it.
type Account struct {
	ID         string   `json:"id"`
	Provider   string   `json:"provider"`
	Email      string   `json:"email"`
	Plan       string   `json:"plan,omitempty"`
	Active     bool     `json:"active"`
	ClaudeCode bool     `json:"claude_code,omitempty"`
	Health     string   `json:"health,omitempty"`
	Usage      []Window `json:"usage"`
	// BankedResets counts the account's unused usage-limit resets (Codex).
	BankedResets int `json:"banked_resets,omitempty"`
	// UsageLimit is the share of the weekly or monthly usage the account
	// keeps; AtLimit says it reached it, so Switcher moved away from it.
	UsageLimit int  `json:"usage_limit,omitempty"`
	AtLimit    bool `json:"at_limit,omitempty"`
	nextReset  string
	// loginKept: Claude Code is signed in with this account, but its requests
	// use the selected one.
	loginKept bool
}

// Window is one usage window, such as a five-hour session or a week.
type Window struct {
	Label       string  `json:"label"`
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at,omitempty"`
}

type serverState struct {
	Version    string `json:"version"`
	HandedBack bool   `json:"handed_back,omitempty"`
	Update     *struct {
		Latest    string `json:"latest"`
		Available bool   `json:"update_available"`
	} `json:"update"`
	Order        []string       `json:"order"`
	Accounts     []apiAccount   `json:"accounts"`
	DesktopRelay map[string]any `json:"desktop_relay"`
}

// apiAccount is one account as the server's API sends it: in the state, and
// in the answers to a login import or an added key.
type apiAccount struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	Email        string `json:"email"`
	Plan         string `json:"plan"`
	Active       bool   `json:"active"`
	NativeActive bool   `json:"native_active"`
	NativeSwitch bool   `json:"native_switch_available"`
	Usage        *struct {
		Windows []Window `json:"windows"`
	} `json:"usage"`
	Health *struct {
		Condition string `json:"condition"`
	} `json:"health"`
	ResetCredits *struct {
		Count  int    `json:"count"`
		NextID string `json:"next_id"`
	} `json:"reset_credits"`
	UsageLimit int  `json:"usage_limit"`
	AtLimit    bool `json:"at_limit"`
}

func (a apiAccount) account() Account {
	account := Account{ID: a.ID, Provider: a.Provider, Email: a.Email, Plan: a.Plan, Active: a.Active, ClaudeCode: a.NativeActive, loginKept: a.NativeActive && !a.NativeSwitch, Usage: []Window{},
		UsageLimit: a.UsageLimit, AtLimit: a.AtLimit && a.UsageLimit > 0}
	if a.Usage != nil && a.Usage.Windows != nil {
		account.Usage = a.Usage.Windows
	}
	if a.Health != nil {
		account.Health = a.Health.Condition
	}
	if a.ResetCredits != nil {
		account.BankedResets, account.nextReset = a.ResetCredits.Count, a.ResetCredits.NextID
	}
	return account
}

// added is the account a login import or an added key answers with, or nil.
func (a *apiAccount) added() *Account {
	if a == nil {
		return nil
	}
	account := a.account()
	return &account
}

func (c *ctx) state() (serverState, []Account, error) {
	var st serverState
	if err := c.cl.call(http.MethodGet, "/api/state", nil, &st); err != nil {
		return st, nil, err
	}
	accounts := make([]Account, 0, len(st.Accounts))
	for _, a := range st.Accounts {
		accounts = append(accounts, a.account())
	}
	// Providers in the dashboard's order, active account first.
	rank := func(p string) int {
		if i := slices.Index(st.Order, p); i >= 0 {
			return i
		}
		return len(st.Order)
	}
	slices.SortStableFunc(accounts, func(a, b Account) int {
		if d := rank(a.Provider) - rank(b.Provider); d != 0 {
			return d
		}
		if a.Provider != b.Provider {
			return strings.Compare(a.Provider, b.Provider)
		}
		if a.Active != b.Active {
			if a.Active {
				return -1
			}
			return 1
		}
		return 0
	})
	return st, accounts, nil
}

// resolve finds one account by id, provider:email, email, the start of its id
// or a unique part of its email.
func resolve(ref string, accounts []Account) (Account, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Account{}, usagef("missing account")
	}
	for _, a := range accounts {
		if a.ID == ref {
			return a, nil
		}
	}
	provider, email, scoped := strings.Cut(ref, ":")
	matchers := []func(Account) bool{
		func(a Account) bool { return scoped && a.Provider == provider && strings.EqualFold(a.Email, email) },
		func(a Account) bool { return strings.EqualFold(a.Email, ref) },
		func(a Account) bool { return strings.HasPrefix(a.ID, ref) },
		func(a Account) bool { return strings.Contains(strings.ToLower(a.Email), strings.ToLower(ref)) },
	}
	for _, match := range matchers {
		var found []Account
		for _, a := range accounts {
			if match(a) {
				found = append(found, a)
			}
		}
		switch len(found) {
		case 0:
			continue
		case 1:
			return found[0], nil
		}
		names := make([]string, len(found))
		for i, a := range found {
			names[i] = fmt.Sprintf("%s:%s (%s)", a.Provider, a.Email, a.ID)
		}
		return Account{}, fmt.Errorf("%q matches %d accounts: %s", ref, len(found), strings.Join(names, ", "))
	}
	return Account{}, fmt.Errorf("no account matches %q; run 'switcher accounts' to list them", ref)
}

// usageText says each window's share left, as the dashboard and the menu
// do; --json keeps used_percent.
func usageText(windows []Window) string {
	parts := make([]string, 0, len(windows))
	for _, w := range windows {
		parts = append(parts, fmt.Sprintf("%s %.0f%% left", w.Label, max(0, 100-w.UsedPercent)))
	}
	return strings.Join(parts, "  ")
}

func healthText(condition string) string {
	switch condition {
	case "", "usage_current", "checking":
		return ""
	case "needs_relogin":
		return "needs relogin"
	case "usage_unavailable":
		return "usage unavailable"
	}
	return strings.ReplaceAll(condition, "_", " ")
}

// accountTable lists accounts by provider, the active one marked.
func accountTable(accounts []Account) string {
	if len(accounts) == 0 {
		return "No accounts yet. Add one with: switcher login claude"
	}
	var rows [][]string
	last := ""
	for _, a := range accounts {
		if a.Provider != last {
			if last != "" {
				rows = append(rows, nil)
			}
			rows = append(rows, []string{providerName(a.Provider)})
			last = a.Provider
		}
		mark := " "
		if a.Active {
			mark = "●"
		}
		notes := []string{}
		switch {
		case a.loginKept:
			// Claude Code is signed in here, but uses the account marked ●.
			notes = append(notes, "Claude Code's login")
		case a.ClaudeCode:
			notes = append(notes, "Claude Code")
		}
		if h := healthText(a.Health); h != "" {
			notes = append(notes, h)
		}
		switch {
		case a.AtLimit:
			notes = append(notes, fmt.Sprintf("at its %d%% limit", a.UsageLimit))
		case a.UsageLimit > 0:
			notes = append(notes, fmt.Sprintf("keeps %d%%", a.UsageLimit))
		}
		plan := planNames[a.Plan]
		if plan == "" {
			plan = a.Plan
		}
		row := []string{"  " + mark + " " + a.Email, plan, usageText(a.Usage)}
		if len(notes) > 0 {
			row = append(row, "· "+strings.Join(notes, ", "))
		}
		rows = append(rows, row)
	}
	return table(rows)
}

func runAccounts(c *ctx, args []string) error {
	fs := c.flags("accounts")
	provider := fs.String("provider", "", "")
	if _, err := c.parse(fs, args, 0, 0); err != nil {
		return err
	}
	_, accounts, err := c.state()
	if err != nil {
		return err
	}
	if *provider != "" {
		if err := knownProvider(*provider); err != nil {
			return err
		}
		accounts = slices.DeleteFunc(accounts, func(a Account) bool { return a.Provider != *provider })
	}
	c.result(map[string]any{"accounts": accounts}, accountTable(accounts))
	return nil
}

func runUse(c *ctx, args []string) error {
	pos, err := c.parse(c.flags("use"), args, 1, 1)
	if err != nil {
		return err
	}
	_, accounts, err := c.state()
	if err != nil {
		return err
	}
	account, err := resolve(pos[0], accounts)
	if err != nil {
		return err
	}
	var answer struct {
		Host   string `json:"host"` // the host whose Claude Code a switch from here changes
		Native *struct {
			Changed bool   `json:"changed"`
			Message string `json:"message"`
		} `json:"native"`
	}
	if err := c.cl.call(http.MethodPost, "/api/accounts/"+url.PathEscape(account.ID)+"/activate", nil, &answer); err != nil {
		return err
	}
	account.Active = true
	switched := answer.Native != nil && answer.Native.Changed
	human := fmt.Sprintf("%s now uses %s.", providerName(account.Provider), account.Email)
	switch {
	case answer.Host != "" && answer.Native != nil:
		human += " " + answer.Native.Message // about the host's Claude Code
	case switched:
		human += " Claude Code switched too."
	case answer.Native != nil:
		human += " Claude Code already used it."
	}
	out := map[string]any{"account": account, "claude_code_switched": switched}
	if answer.Host != "" {
		out["claude_code_host"] = answer.Host // whose Claude Code claude_code_switched is about
	}
	c.result(out, human)
	return nil
}

const limitHelp = `Keeps a share of an account's weekly or monthly usage for what Switcher
cannot route, such as the Claude and ChatGPT chat apps. Once the account has
that much or less left, Switcher moves its provider to the account with the
most usage left and does not move back to it on its own. With no other
account to move to, it keeps using this one.

  switcher limit me@example.com 10     keep 10%: switch away at 10% left
  switcher limit me@example.com off    use it all again
  switcher limit me@example.com        show its limit

On a Mac that uses another Switcher's accounts, the limit is set on that
Switcher and applies everywhere.
`

func runLimit(c *ctx, args []string) error {
	pos, err := c.parse(c.flags("limit"), args, 1, 2)
	if err != nil {
		return err
	}
	_, accounts, err := c.state()
	if err != nil {
		return err
	}
	account, err := resolve(pos[0], accounts)
	if err != nil {
		return err
	}
	if len(pos) == 1 {
		human := fmt.Sprintf("%s has no usage limit.", account.Email)
		switch {
		case account.AtLimit:
			human = fmt.Sprintf("%s keeps %d%% of its usage and has reached that.", account.Email, account.UsageLimit)
		case account.UsageLimit > 0:
			human = fmt.Sprintf("%s keeps %d%% of its usage: Switcher moves away at %d%% left.", account.Email, account.UsageLimit, account.UsageLimit)
		}
		c.result(map[string]any{"account": account}, human)
		return nil
	}
	limit, err := parseLimit(pos[1])
	if err != nil {
		return err
	}
	var answer struct {
		Account *apiAccount `json:"account"`
	}
	if err := c.cl.call(http.MethodPatch, "/api/accounts/"+url.PathEscape(account.ID), map[string]int{"usage_limit": limit}, &answer); err != nil {
		return err
	}
	if updated := answer.Account.added(); updated != nil {
		account = *updated
	} else {
		account.UsageLimit = limit
	}
	human := fmt.Sprintf("%s has no usage limit now.", account.Email)
	if limit > 0 {
		human = fmt.Sprintf("%s keeps %d%%: Switcher moves %s to another account at %d%% left.", account.Email, limit, providerName(account.Provider), limit)
	}
	c.result(map[string]any{"account": account}, human)
	return nil
}

// parseLimit reads a usage limit: a percentage from 1 to 95, with or without
// the percent sign, or off (0).
func parseLimit(text string) (int, error) {
	text = strings.TrimSuffix(strings.TrimSpace(strings.ToLower(text)), "%")
	if text == "off" || text == "none" || text == "0" {
		return 0, nil
	}
	var limit int
	if _, err := fmt.Sscanf(text, "%d", &limit); err != nil || fmt.Sprint(limit) != text || limit < 1 || limit > 95 {
		return 0, usagef("a limit is a percentage from 1 to 95, or off")
	}
	return limit, nil
}

func runRemove(c *ctx, args []string) error {
	fs := c.flags("remove")
	yes := fs.Bool("yes", false, "")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	_, accounts, err := c.state()
	if err != nil {
		return err
	}
	account, err := resolve(pos[0], accounts)
	if err != nil {
		return err
	}
	if !*yes {
		return usagef("this removes %s:%s from Switcher; add --yes to confirm", account.Provider, account.Email)
	}
	if err := c.cl.call(http.MethodDelete, "/api/accounts/"+url.PathEscape(account.ID), nil, nil); err != nil {
		return err
	}
	c.result(map[string]any{"removed": account}, fmt.Sprintf("Removed %s account %s.", providerName(account.Provider), account.Email))
	return nil
}

const resetHelp = `Spends one banked usage-limit reset (Codex) on an account, so it can be used
again right away. Name the account, or a provider for its active account:

  switcher reset codex --yes
  switcher reset me@example.com --yes

Banked resets cannot be given back, so the command asks for --yes.
`

func runReset(c *ctx, args []string) error {
	fs := c.flags("reset")
	yes := fs.Bool("yes", false, "")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	_, accounts, err := c.state()
	if err != nil {
		return err
	}
	var account Account
	if _, provider := providerNames[pos[0]]; provider {
		found := false
		for _, a := range accounts {
			if a.Provider == pos[0] && a.Active {
				account, found = a, true
			}
		}
		if !found {
			return fmt.Errorf("no %s account is in use", providerName(pos[0]))
		}
	} else if account, err = resolve(pos[0], accounts); err != nil {
		return err
	}
	if account.nextReset == "" {
		return fmt.Errorf("%s has no banked reset", account.Email)
	}
	if !*yes {
		plural := "s"
		if account.BankedResets == 1 {
			plural = ""
		}
		return usagef("this spends one of the %d banked reset%s of %s; add --yes to confirm", account.BankedResets, plural, account.Email)
	}
	var answer struct {
		Outcome string `json:"outcome"`
		Usage   *struct {
			Windows []Window `json:"windows"`
		} `json:"usage"`
		Account *struct {
			ResetCredits *struct {
				Count int `json:"count"`
			} `json:"reset_credits"`
		} `json:"account"`
	}
	if err := c.cl.call(http.MethodPost, "/api/accounts/"+url.PathEscape(account.ID)+"/use-reset", map[string]string{"credit_id": account.nextReset}, &answer); err != nil {
		return err
	}
	left := 0
	if answer.Account != nil && answer.Account.ResetCredits != nil {
		left = answer.Account.ResetCredits.Count
	}
	human := map[string]string{
		"reset":            fmt.Sprintf("Used a banked reset on %s.", account.Email),
		"already_redeemed": fmt.Sprintf("That reset was already used on %s.", account.Email),
		"nothing_to_reset": fmt.Sprintf("%s has nothing to reset right now; the reset was kept.", account.Email),
	}[answer.Outcome]
	if human == "" {
		human = fmt.Sprintf("The provider answered %q for %s.", answer.Outcome, account.Email)
	}
	human += fmt.Sprintf(" %d banked reset%s left.", left, map[bool]string{true: "", false: "s"}[left == 1])
	c.result(map[string]any{"account": account.ID, "outcome": answer.Outcome, "banked_resets_left": left}, human)
	return nil
}

func runRefresh(c *ctx, args []string) error {
	pos, err := c.parse(c.flags("refresh"), args, 0, 1)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		if err := c.cl.call(http.MethodPost, "/api/usage/refresh", nil, nil); err != nil {
			return err
		}
		c.result(map[string]any{"refreshed": "all"}, "Refreshing usage for every account.")
		return nil
	}
	_, accounts, err := c.state()
	if err != nil {
		return err
	}
	account, err := resolve(pos[0], accounts)
	if err != nil {
		return err
	}
	if err := c.cl.call(http.MethodPost, "/api/accounts/"+url.PathEscape(account.ID)+"/refresh", nil, nil); err != nil {
		return err
	}
	c.result(map[string]any{"refreshed": account.ID}, fmt.Sprintf("Refreshed usage for %s.", account.Email))
	return nil
}
