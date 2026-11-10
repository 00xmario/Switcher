---
name: switcher
description: Drive the Switcher app on this computer, which holds the user's AI subscription accounts (Claude, Codex/ChatGPT, Grok, OpenCode, Gemini, Copilot) and routes Claude Code, Claude Desktop and Codex through the one selected per provider. Use it whenever the user talks about Switcher, switching or picking a Claude/Codex/ChatGPT account, usage or quota left, weekly limits, resets, keeping a percentage on an account, switching automatically when an account reaches some percentage, banked resets, sharing accounts with another computer, or how busy this computer or the Switcher host is.
metadata:
  managed-by: switcher
---

# Switcher

Switcher is a local app (menu bar on macOS, tray on Linux, dashboard at
http://127.0.0.1:8787) that keeps several subscription accounts per provider
and routes the user's tools through one selected account per provider. For
Claude it also switches Claude Code's own login. It moves to another account
by itself when the selected one runs out, or reaches a usage limit the user
set. You control it with the `switcher` command.

## Ground rules

- Use the `switcher` command, always with `--json`, and read its JSON. Exit
  codes: `0` ok, `1` failed, `2` wrong usage, `3` Switcher is not running
  (then run `switcher start` and retry once). With `--json` a failure prints
  `{"error": "...", "code": "..."}`; tell the user the error in plain words.
- Look before you act: run `switcher accounts --json` to see the accounts,
  their ids, emails, plans and usage. Never guess an account.
- An `<account>` argument is the account's id, its email, `provider:email`
  (when one email has accounts with several providers), the start of its id,
  or a unique part of its email ("outlook", "work").
- Never read files under `~/.switcher`, never print tokens or keys, and never
  edit Switcher's files by hand. Everything goes through the command.
- Commands that change something the user cannot undo need `--yes`. Pass
  `--yes` only when the user asked for exactly that action in this
  conversation: `remove`, `reset` (spends a banked reset), `disconnect`,
  `hand-back on`, `desktop restart`, `share revoke`, `away remove`.
- Usage is shown as **percent left**. In JSON each usage window has `label`
  ("Session" is the 5-hour window, "Weekly", "Monthly", or a model's own
  window), `used_percent` and `resets_at` (Unix seconds). Left is
  `100 - used_percent`. Talk to the user in "left", as Switcher does.

## Usage limits: keep a share and switch automatically

A usage limit makes an account keep a share of its weekly (or monthly) usage,
for things Switcher cannot route, such as the Claude and ChatGPT chat apps
and websites. When the account has that much or less left, Switcher moves
its provider to the account with the most usage left: Claude Code's login and
Claude Desktop for Claude, the proxy for Codex and the others. It does not
switch onto an account that is at its own limit (unless every other account
is out, so requests keep working), and does not move back by itself.

```sh
switcher limit <account> 10 --json     # keep 10%: switch away at 10% left
switcher limit <account> off --json    # no limit: the account may use everything
switcher limit <account> --json        # show the account's limit
```

The number is always the share **left** at which Switcher switches away,
from 1 to 95. Translate what the user says:

| The user says | Run |
|---|---|
| "When my account reaches 10%, switch to my other account" | `switcher limit <account> 10` |
| "Keep 10% in this account and auto switch" | `switcher limit <account> 10` |
| "Don't let my work account go below 20%" | `switcher limit work 20` |
| "Save a quarter of my Claude Max for chat" | `switcher limit <account> 25` |
| "Only let it use 30% of its weekly usage" / "stop after it used 30%" | `switcher limit <account> 70` (used 30% = 70% left) |
| "Let it burn everything again" / "remove the limit" | `switcher limit <account> off` |

How to carry it out:

1. Find the account. "This account" or "my current account" is the selected
   one: `active: true` (for Claude, `claude_code: true` is the one Claude Code
   is signed in with). "My Outlook account" matches the email. If two
   accounts could match, ask which one; never pick one at random.
2. "Reaches 10%" and "keep 10%" both mean 10% **left**. Only when the user
   clearly speaks of usage spent ("used", "burned", "after 30%") turn it
   around: limit = 100 - spent. If you cannot tell, ask in one short question:
   "Switch away when it has 10% left, or after it used 10%?"
3. The other account needs no setting: Switcher picks the account of the same
   provider with the most usage left. A specific destination is not
   something a limit sets; if the user insists, explain that, and offer
   `switcher use <account>` for now or limits on the accounts they want spared.
4. Run the command, then confirm from its JSON (`usage_limit`, `at_limit`),
   and tell the user in one sentence, for example: "Done: when
   work@outlook.com is down to 10% of its weekly usage, Switcher moves Claude
   to your account with the most left."
5. Mention what matters for the case:
   - An account already at or below the new limit is switched away from
     right away, if another account has room.
   - With no other account that has room, Switcher keeps using this one: a
     limit never blocks requests.
   - Limits watch the weekly or monthly window, not the 5-hour session or a
     model's own window. An account with neither cannot have a limit that
     does anything; `switcher accounts --json` shows its windows.
   - A limit works whether or not `auto_switch_claude` is on; that setting
     is about accounts that run out completely.
   - If the user switches to an account at its limit themselves, Switcher
     leaves it there for an hour, then moves on again.
   - On a computer that uses another computer's accounts (a paired host),
     the limit is saved on the host and applies to every computer using it.

Set limits on several accounts by running the command once per account.

## Everyday commands

| Task | Command |
|---|---|
| Overview: accounts, usage, sharing, Claude Desktop | `switcher status --json` |
| Accounts with usage (optionally one provider) | `switcher accounts [--provider claude] --json` |
| Use an account now (Claude also switches Claude Code) | `switcher use <account> --json` |
| Usage limit | `switcher limit <account> [percent\|off] --json` |
| Refresh usage now | `switcher refresh [account] --json` |
| How busy this computer, or the host, is | `switcher load --json` |
| What is using it, by app (processor, memory, graphics) | `switcher load --apps --json` |
| Install this skill on this computer and its host | `switcher skill install --on both --json` |
| Spend a banked reset (Codex), only when asked | `switcher reset <account> --yes --json` |
| Settings | `switcher settings --json`, `switcher settings set auto_switch_claude=false --json` |
| Sign in a new account (opens a browser) | `switcher login <provider>` |
| Open the dashboard | `switcher open` |
| Help for one command | `switcher help <command>` |

Settings keys: `auto_switch_claude` (move Claude off an account that runs
out, default on), `auto_use_reset` (spend a Codex banked reset automatically
before falling back to a Free account or one at its usage limit), `menu_usage_bars`,
`reset_notifications`, `compact_accounts`, `merge_accounts`.

## Answering questions

- "How much do I have left?" Read `switcher accounts --json` and answer per
  account in percent left, with when it resets (`resets_at`).
- "Which account am I on?" The account with `active: true` per provider; for
  Claude also say which one Claude Code uses (`claude_code: true`).
- "Why did it switch?" An account ran out, or reached its usage limit
  (`at_limit: true`). Show the limits with `switcher accounts --json`.
- "Is the server busy?" `switcher load --json` gives `cpu`, `memory` and
  `gpu` in percent, `cores`, `memory_used`/`memory_total` in bytes;
  `remote: true` means it is the paired host's load (`machine` names it).
- "What is using it?" `switcher load --apps --json` adds `apps`: each app's
  `name`, `processes`, `cpu` (percent of the whole machine), `memory`
  (bytes) and, when `gpu_apps` is true, `gpu` (percent) and `gpu_memory`.
  `apps_unavailable: true` means the listing failed this time (then `apps`
  and `processes` say nothing): try again.
  Only apps using something are listed. The dashboard's Load page shows the
  same, live.

## Sharing between computers

A computer can use another computer's accounts ("the host"). Then accounts,
usage, switches and limits are the host's, and commands run on either
computer act on the host's accounts. `switcher status --json` shows the
pairing under `sharing`. Pairing and unpairing (`connect`, `disconnect`,
`share`) change how the user's tools work: do them only when asked.

## Do not

- Do not remove accounts, spend banked resets, hand tools back, disconnect a
  host or restart Claude Desktop unless the user asked for that exact thing.
- Do not log out of Claude Code (`/logout`) to switch accounts; it can revoke
  the saved login. Use `switcher use`.
- Do not edit `~/.claude`, `~/.codex` or `~/.switcher` to switch accounts.
