# borggen

A small web constructor for [BorgBackup](https://borgbackup.readthedocs.io)
job scripts. Fill in a form, get a ready-to-run bash script with syntax
highlighting, save it as a `.sh` file.

It generates the **routine part** of a backup job — transport, repository,
sources, excludes, retention, notifications, execution-safety plumbing
(locking, logging, signal handling, a single notification point). Anything
job-specific (database dumps, stopping a service before backup) is
hand-written in zones that survive regenerating the script from the form.

## Features

- Local, `sshfs` (pull), or `cifs` (SMB) sources; local or `ssh://` (push)
  repositories
- Retention (`prune`/`compact`) and a lightweight post-backup `borg check`
- Telegram, email (SMTP via `curl`), and webhook (e.g. uptime-kuma)
  notifications, with per-channel log attachment rules
- Three run modes baked into every generated script: `--check` (environment
  only), `--dry-run`, and a real run — never separate test/production files
- Manual edits to the generated script are preserved across regeneration via
  named hand-edit zones
- Optional git-backed library for saved jobs, with an optional push to a
  remote git server
- No database — everything is a flat file or a local git repository

## Requirements

Docker with the `compose` plugin. Go does not need to be installed locally —
the build happens inside the Docker image.

## Quick start

```bash
git clone <this-repo>
cd borggen
docker build -t borggen .
docker run -d --name borggen -p 8080:8080 -v borggen-data:/data borggen
```

Open `http://localhost:8080`.

To run it behind an existing reverse proxy instead, copy
`compose.yaml.example` to `compose.yaml`, adjust the routing labels for your
setup, and `docker compose up -d --build`.

## Notes

- Secrets entered in the form (repository passphrase, notification tokens)
  are currently written in clear text into the generated script and, if you
  use the git library feature, into that repository's history. Treat the
  library's storage location and any git remote you push it to accordingly.
- `compose.yaml.example` mentions an optional `BORGMON_URL` variable. It
  points at borgmon, a companion monitoring service that is not part of this
  repository and is not published; leave the variable unset and the feature
  is hidden from the UI entirely, with no trace in any generated script.
- borggen does not run backups, schedule anything, or touch the target
  repository itself — it only writes the script. Scheduling (cron, a systemd
  timer) and running `borg init` on the repository are up to you.
