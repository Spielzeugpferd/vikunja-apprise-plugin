# vikunja-apprise

A [Vikunja](https://github.com/go-vikunja/vikunja) plugin that bridges task reminders and
notifications to [Apprise](https://github.com/caronc/apprise) via a self-hosted
[Apprise API](https://github.com/caronc/apprise-api) instance — giving you delivery to 100+
services (Telegram, Pushover, ntfy, Discord, Gotify, PushBullet, Matrix, email, SMS, ...)
without forking or patching Vikunja itself.

It's a source plugin for Vikunja's [Yaegi plugin loader](https://vikunja.io/docs/plugins/) —
Vikunja interprets `main.go` at startup, no compiled binary and no Vikunja fork required.

## What it does

- Exposes three authenticated routes on your Vikunja instance for managing your own notification
  targets: `POST/GET/DELETE /api/v1/plugins/apprise/config`. These proxy to a config key
  (`vikunja-user-<your user id>` by default) on your Apprise API instance — no notification
  secrets (bot tokens, webhook URLs, SMTP credentials) are stored in Vikunja's own database.
- Listens on Vikunja's internal event bus for task reminders, overdue tasks, and every other
  notification type (comments, assignments, mentions, project/team events), and forwards each one
  to your Apprise config as a `POST /notify/<key>` call.

## Requirements

- A Vikunja instance with `plugins.enabled: true` and `plugins.loader: yaegi`.
- A self-hosted [Apprise API](https://github.com/caronc/apprise-api) instance, reachable from the
  Vikunja backend on an **internal-only** network. See "Security" below — this is not optional.

## Installation

1. Copy (or, for local development, symlink) this directory into Vikunja's plugin directory, e.g.
   `plugins/vikunja-apprise/` relative to Vikunja's `service.rootpath` (`plugins.dir` if you've
   changed the default).
2. Run Apprise API somewhere only your Vikunja backend can reach — see `docker-compose.yml` in
   this repo for a local example.
3. Point the plugin at it and enable the Vikunja plugin system:

   ```bash
   VIKUNJA_PLUGINS_ENABLED=true
   VIKUNJA_PLUGINS_LOADER=yaegi
   VIKUNJA_PLUGINS_APPRISE_APIURL=http://apprise:8000   # wherever your Apprise API instance lives
   ```

4. Restart Vikunja. There is no hot reload — the plugin directory is only read at startup.

## Usage

Set your notification targets (any valid [Apprise URL](https://github.com/caronc/apprise#popular-notification-services)):

```bash
curl -X POST https://your-vikunja-instance/api/v1/plugins/apprise/config \
  -H "Authorization: Bearer $VIKUNJA_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"urls": ["tgram://bottoken/ChatID", "ntfy://topic"]}'
```

From then on, task reminders and other Vikunja notifications for that user are forwarded to
whichever services you configured.

## Security

**Apprise API has no built-in authentication** on `/add`, `/notify`, etc. — that's by design on
their side, meant for exactly this kind of internal-only deployment. It must never be reachable
directly from end users or the public internet. This plugin's authenticated routes (behind
Vikunja's own session/JWT auth) are the only sanctioned way for a user to read or change their own
config key. Do not expose the Apprise API port publicly, and do not add an unauthenticated route to
this plugin that forwards user input straight through to it.

## Development

See [AGENTS.md](./AGENTS.md) for the architecture rationale, Yaegi runtime constraints, and the
local dev/test workflow.

## License

MIT — see [LICENSE](./LICENSE). This plugin is not affiliated with the Vikunja or Apprise
projects; it's a small bridge between two independently licensed open-source tools (Vikunja:
AGPL-3.0, Apprise/Apprise API: BSD-2-Clause/MIT). Running this plugin against your own Vikunja
instance over its public API/event bus does not modify Vikunja's own source.
