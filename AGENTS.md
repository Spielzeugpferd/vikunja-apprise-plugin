# vikunja-apprise — Agent Notes

## Purpose

A Yaegi source plugin for [Vikunja](https://github.com/go-vikunja/vikunja) that forwards task
reminders and other notifications to a self-hosted [Apprise API](https://github.com/caronc/apprise-api)
instance, which fans them out to 100+ notification services — without forking or patching Vikunja.
Published as its own repository so it can be shared independently of any particular Vikunja
checkout; see `README.md` for the user-facing installation and usage docs.

## Repository layout

This repo is self-contained:

- `main.go` — the plugin (`//go:build ignore`, package `main`). This is the only Go source file;
  all `.go` files directly in this directory are evaluated together by Vikunja's Yaegi loader as
  package `main`, so keep it that way if this ever grows into multiple files.
- `docker-compose.yml` — a local Apprise API instance for development/testing.
- `README.md` — user-facing installation/usage/security docs.

It assumes a sibling checkout of Vikunja for local testing (see "Local development" below), but
does not itself contain or depend on Vikunja's source beyond what Vikunja's Yaegi loader exposes
at runtime (see "Runtime constraints").

## Architecture

Three fork-free ways to extend Vikunja were considered for this feature: a standalone microservice
consuming Vikunja's webhook feature, a small additive patch to Vikunja core (upstreamable), and an
in-process Yaegi plugin. This project is the **Yaegi plugin** approach: for a self-hosted Vikunja
instance it needs no extra hosting on the Vikunja side, reuses Vikunja's own authenticated router
and session auth for its HTTP endpoints, and can listen directly on Vikunja's internal event bus —
including `notification.created`, which fires for every notification type Vikunja has. A
webhook-based microservice would only receive the two events Vikunja currently exposes as
user-directed webhooks (`task.reminder.fired`, `task.overdue`); this plugin gets full coverage.

Trade-off accepted: Vikunja's Yaegi loader is a newer, less battle-tested subsystem, and a plugin
can only import Go's standard library plus Vikunja's curated `pkg/yaegi_symbols` packages — no
third-party Go modules (see "Runtime constraints").

### Why Apprise as the delivery layer

An earlier prototype of this plugin hand-rolled Web Push (RFC 8291/8292) directly to browsers using
stdlib crypto. That covered exactly one channel, and hand-written crypto is hard to trust without
extensive live testing. [Apprise](https://github.com/caronc/apprise) (BSD-2-Clause) plus its
companion [Apprise API](https://github.com/caronc/apprise-api) (MIT) solve this generically: Apprise
API is a small, self-hostable REST service that fans a single notify call out to 100+ services
(Telegram, Discord, Slack, Pushover, ntfy, Gotify, PushBullet, email, SMS, Matrix, etc.) via a
URL scheme. Several of those deliver real push notifications to a phone today, via client apps
someone else maintains. This plugin implements **no push protocol itself** — only:

1. Authenticated routes that let a Vikunja user manage their own Apprise config key
   (`POST/GET/DELETE /plugins/apprise/config`, proxied to Apprise API's `/add`, `/json/urls`,
   `/del`).
2. Event listeners that call `POST /notify/<key>` on the same Apprise API instance with a
   title/body whenever a relevant Vikunja event fires.

### Event coverage and why there are four listeners, not one

- `task.reminder.fired` (`models.TaskReminderFiredEvent`) — dispatched unconditionally (gated only
  by the instance-wide `webhooks.enabled` setting), independent of the user's own
  email-reminder preference. This is the reliable hook for reminder pushes; the DB-backed
  `ReminderDueNotification` (which would otherwise reach `notification.created`) is only persisted
  when the user has email reminders enabled, so it can't be relied on alone.
- `task.overdue` / `tasks.overdue` — `UndoneTaskOverdueNotification`/`UndoneTasksOverdueNotification`
  have `ToDB()` return `nil`, so they never reach the `notifications` table and never fire
  `notification.created`. These dedicated events are the only hook for overdue pushes.
- `notification.created` — the generic catch-all for everything that *does* get persisted
  (comments, assignments, mentions, deletions, project/team events). The handler explicitly skips
  `Name == "task.reminder"` to avoid double-sending, since that case is already fully handled by
  the dedicated reminder listener above.

## Security model

Apprise API has **no built-in authentication** on `/add`, `/notify`, etc. — by design on their
side. It must never be reachable directly from end users or the public internet; it belongs on an
internal-only network reachable only from the Vikunja backend. This plugin's authenticated routes
(under Vikunja's own JWT/session auth) are the *only* sanctioned entry point for end users to read
or change their own config key. Never add an unauthenticated route here that forwards
user-controlled input straight to Apprise API.

Apprise URLs can embed bot tokens, webhook secrets, SMTP credentials, etc. Storing them in Apprise
API's own config store — rather than duplicating them into Vikunja's database — keeps that secret
material out of Vikunja's data at rest entirely; this plugin holds no secrets of its own.

## Runtime constraints (Yaegi)

- Only Go's standard library (`stdlib.Symbols`) and Vikunja's exposed packages
  (`pkg/yaegi_symbols/*.go`: `db`, `events`, `log`, `models`, `plugins`, `user`, `config`, plus
  `xorm.io/xorm`, `src.techknowlogick.com/xormigrate`, `github.com/labstack/echo/v5`,
  `github.com/ThreeDotsLabs/watermill/message`) are importable. No third-party Go modules —
  `pkg/notifications` itself is *not* exposed, which is why the generic listener registers on the
  event's well-known string name (`"notification.created"`) instead of importing the real event
  type.
- All `.go` files directly inside this directory are evaluated together as `package main`. Every
  file needs `//go:build ignore` as the first line so a `go vet ./...`/`go build ./...` at the
  Vikunja repo root would skip it if this repo were ever nested there (it isn't, by design, but the
  loader itself doesn't care about that tag — only Go's own toolchain does).
- Required exported factories: `NewPlugin() plugins.Plugin` (always), plus a typed factory for any
  optional capability actually implemented (here: `NewAuthenticatedRouterPlugin()`). Yaegi wraps
  return values by declared type, so a plain type assertion from `Plugin` to a sub-interface does
  not work — see `examples/plugins/example/main.go` in the Vikunja repo for the canonical pattern.
- Interpreted struct types reach xorm as anonymous reflect structs with no methods: `TableName()`
  is invisible, so every query passes the table name explicitly via `.Table("...")`.
- If a future change needs a Vikunja-internal package that isn't yet exposed, that requires running
  `mage generate:yaegi-symbols` inside the Vikunja checkout (regenerates `pkg/yaegi_symbols/*.go`)
  — a change to Vikunja's own repo, not this one. Prefer working within what's already exposed.

## Local development

This repo expects a sibling Vikunja checkout for testing, e.g.:

```
some-parent-dir/
├── vikunja/            # https://github.com/go-vikunja/vikunja, native toolchain via mise.toml
└── vikunja-apprise/     # this repo
```

Toolchain: `cd vikunja && mise install` (pins Go/Node/pnpm to what `go.mod`/`frontend/package.json`
expect).

Mount point: create a real directory under Vikunja's plugin dir (Yaegi's loader requires an actual
directory there, not a symlink — `os.ReadDir` + `DirEntry.IsDir()` does not resolve symlinks), and
symlink just the `.go` file(s) inside it back to this repo, so edits here are picked up without a
copy step:

```bash
mkdir -p ../vikunja/plugins/vikunja-apprise
ln -sf ../../../vikunja-apprise/main.go ../vikunja/plugins/vikunja-apprise/main.go
```

All of the above (Apprise API, the plugin symlink, and starting Vikunja) is wrapped in
`scripts/dev-up.sh`:

```bash
./scripts/dev-up.sh
```

It also creates a placeholder `frontend/dist/index.html` in the Vikunja checkout if missing —
`frontend/embed.go` uses `//go:embed all:dist`, which requires that directory to exist even for a
backend-only dev run with no real frontend build — and sets `VIKUNJA_SERVICE_PUBLICURL`, which
Vikunja requires whenever `cors.enable` is true (the default).

There is **no hot reload** — the Yaegi loader reads the plugin directory once at startup
(`pkg/plugins/manager.go` in Vikunja). Restart the process (re-run the script) after every change
to `main.go`.

## Testing

Because this plugin lives outside the Vikunja module, its automated smoke test lives inside the
Vikunja checkout instead (it needs Vikunja's own internal `yaegi.LoadPluginFull` test helper, which
isn't importable from outside that module): `vikunja/pkg/plugins/yaegi/apprise_plugin_test.go`,
pointed at this repo via the same relative path the symlink above uses. Run it from the Vikunja
checkout:

```bash
cd ../vikunja
go test ./pkg/plugins/yaegi/... -run Apprise -v
```

That test only proves the plugin loads and its routes are wired correctly (401 without auth, etc.)
— it does not exercise real delivery. To verify actual delivery end-to-end, with Vikunja and
Apprise API both running as above:

```bash
# Get a session/API token from Vikunja first, then:
curl -X POST http://localhost:3456/api/v1/plugins/apprise/config \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"urls": ["json://localhost:9000/hook"]}'   # any apprise:// URL you can observe

curl http://localhost:3456/api/v1/plugins/apprise/config -H "Authorization: Bearer $TOKEN"

# Then trigger a real Vikunja event (e.g. create a task with a reminder a minute out)
# and confirm your target received it.
```

## Status

Implemented and verified end-to-end: loads correctly against a live Vikunja Yaegi plugin loader
(smoke tests above), the config add/get/delete round-trip works against a real local Apprise API
container, and a real Vikunja task reminder (via `task.reminder.fired`) was delivered through
Apprise to a real third-party service (a public [ntfy.sh](https://ntfy.sh) topic, no credentials
needed) — the message arrived with the expected title/body.

Not yet exercised: the `notification.created` catch-all path (comments, assignments, mentions,
etc.) and the overdue-task listeners, against a real delivery target — only the reminder path has
been proven end-to-end so far. The code path is the same, so this is a smaller gap than it sounds,
but worth doing before relying on it.
