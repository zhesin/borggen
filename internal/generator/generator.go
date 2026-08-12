// Package generator turns a BackupJob into a bash script.
//
// The script is assembled from ordered section fragments rather than one big
// template: transport(3) x repo(2) x notify(3) makes a monolithic template
// unreadable. Each unexported section* function emits one fragment, in the
// order fixed by the specification.
package generator

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"borggen/internal/model"
)

// Version is stamped into the header of every generated script.
const Version = "0.8.8"

// Marker lines. The meta block carries the round-trip parameters; the user
// zones carry hand-written code that survives regeneration.
const (
	MetaBegin = "# --- borggen:meta ---"
	MetaEnd   = "# --- borggen:meta end ---"

	UserPreBegin  = "# --- borggen:user:pre BEGIN ---"
	UserPreEnd    = "# --- borggen:user:pre END ---"
	UserPostBegin = "# --- borggen:user:post BEGIN ---"
	UserPostEnd   = "# --- borggen:user:post END ---"

	timestampPrefix = "# Generated: "
)

// Options carries generation-time inputs that are not job parameters.
type Options struct {
	// Now is the header timestamp. Zero means time.Now().
	Now time.Time
	// UserPre and UserPost are hand-written blocks preserved from a previous
	// version of the script.
	UserPre  string
	UserPost string
	// Filename is the on-disk base name LOG_FILE/LOCK_FILE are always derived
	// from — sectionConfig for the main script, companionPaths (in
	// companion.go) for the check/restore-drill companions — independent of
	// Job.Name/BACKUPNAME. Neither is a BackupJob field: there is no override,
	// no UI control, nothing to round-trip through the meta block. Empty
	// falls back to Job.Name (a bare API/test call with no Filename context).
	Filename string
	// LogDir/LockDir override the fixed /var/log/borg, /var/lock/borg
	// directories. Go-level test plumbing only — never read from JSON, never
	// set by httpapi — so a test that actually executes the generated script
	// (script_run_test.go) can point it at a temp directory instead of
	// writing to real system paths. Empty means the real, hardcoded default.
	LogDir  string
	LockDir string
	// MonitorURL is the borgmon base URL. Like Filename it belongs to the
	// deployment rather than to the job: the same for every job on the
	// server, read from the environment at startup, and never round-tripped
	// through a script's meta block.
	MonitorURL string
}

func logDirOr(opts Options) string {
	if opts.LogDir != "" {
		return opts.LogDir
	}
	return "/var/log/borg"
}

func lockDirOr(opts Options) string {
	if opts.LockDir != "" {
		return opts.LockDir
	}
	return "/var/lock/borg"
}

// fileBase is the on-disk name everything that names a *file* derives from:
// LOG_FILE, LOCK_FILE, the companions' suffixed variants, and every place a
// comment tells the reader what to type at a shell (the Usage line, the
// suggested cron entries). BACKUPNAME is the archive prefix and is not
// interchangeable with it — a job renamed after its script was scheduled has
// the two legitimately differ, and printing BACKUPNAME where a filename
// belongs hands the reader a command that does not exist.
func fileBase(job model.BackupJob, opts Options) string {
	if opts.Filename != "" {
		return opts.Filename
	}
	return job.Name
}

// Generate renders the job as a bash script with LF line endings.
func Generate(job model.BackupJob, opts Options) (string, error) {
	job.Normalize()
	if problems := job.Validate(); len(problems) > 0 {
		parts := make([]string, len(problems))
		for i, p := range problems {
			parts[i] = p.String()
		}
		return "", fmt.Errorf("invalid parameters: %s", strings.Join(parts, "; "))
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}

	var b strings.Builder
	sections := []func(*strings.Builder, model.BackupJob, Options) error{
		sectionHeader,
		sectionMeta,
		sectionRunMode,
		sectionConfig,
		sectionSources,
		sectionHelpers,
		sectionNotify,
		sectionCleanupExit,
		sectionTraps,
		// Lock before logging, and both after the traps. After the traps
		// because either one can abort the script, and that abort must still
		// reach the notification channels. Lock *before* logging because the
		// logging setup truncates LOG_FILE: with the old order a second,
		// overlapping run wiped the log of the run already in progress before
		// discovering it could not take the lock (reproduced directly). The
		// log belongs to whichever run holds the lock — see LOG_OWNED.
		sectionLock,
		sectionLogging,
		// After the lock and the logging: a run that lost the lock race must
		// not report itself as started, and the push belongs in the log.
		sectionMonitorStart,
		sectionMount,
		sectionPreflight,
		sectionUserPre,
		sectionBorg,
		sectionUserPost,
		sectionFinish,
	}
	for _, s := range sections {
		if err := s(&b, job, opts); err != nil {
			return "", err
		}
	}
	return normalizeLF(b.String()), nil
}

// --- section fragments ------------------------------------------------------

func sectionHeader(b *strings.Builder, job model.BackupJob, opts Options) error {
	line(b, "#!/bin/bash")
	line(b, "#")
	line(b, "# Backup job: %s", job.Name)
	line(b, "# Generated by borggen %s", Version)
	line(b, "%s%s", timestampPrefix, opts.Now.UTC().Format(time.RFC3339))
	line(b, "# Target borg version: %s", job.BorgTarget)
	line(b, "#")
	line(b, "# Edit only inside the borggen:user zones. Everything else is rewritten")
	line(b, "# when the script is regenerated from the form.")
	line(b, "#")
	line(b, "# Usage: %s.sh [--check|--dry-run]", fileBase(job, opts))
	line(b, "#")
	line(b, "# To stop a run in progress, signal the whole process group, not just this")
	line(b, "# script's PID: bash only reacts to a signal once the current borg command")
	line(b, "# returns, so 'kill <pid>' alone can look hung for as long as that command")
	line(b, "# keeps running. Use 'kill -TERM -- -<pgid>' (note the leading dash), or run")
	line(b, "# this job under systemd with KillMode=control-group and stop it with")
	line(b, "# 'systemctl stop'.")
	blank(b)
	return nil
}

func sectionMeta(b *strings.Builder, job model.BackupJob, _ Options) error {
	raw, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	line(b, "%s", MetaBegin)
	for _, l := range strings.Split(string(raw), "\n") {
		if l == "" {
			line(b, "#")
			continue
		}
		line(b, "# %s", l)
	}
	line(b, "%s", MetaEnd)
	blank(b)
	return nil
}

func sectionRunMode(b *strings.Builder, job model.BackupJob, _ Options) error {
	header(b, "Run mode")
	line(b, `MODE="real"`)
	line(b, `case "${1:-}" in`)
	line(b, `    "")        MODE="real" ;;`)
	line(b, `    --check)   MODE="check" ;;`)
	line(b, `    --dry-run) MODE="dry-run" ;;`)
	line(b, `    -h|--help) printf 'Usage: %%s [--check|--dry-run]\n' "$0"; exit 0 ;;`)
	line(b, `    *)`)
	line(b, `        printf 'Unknown argument: %%s\n' "$1" >&2`)
	line(b, `        printf 'Usage: %%s [--check|--dry-run]\n' "$0" >&2`)
	line(b, `        exit 2`)
	line(b, `        ;;`)
	line(b, `esac`)
	blank(b)
	return nil
}

func sectionConfig(b *strings.Builder, job model.BackupJob, opts Options) error {
	header(b, "Config")
	line(b, "BACKUPNAME=%s", q(job.Name))
	line(b, "ARCHIVE=%s", q(job.ArchiveName()))
	line(b, "BORG_TARGET=%s", q(job.BorgTarget))
	line(b, "# Read once here rather than inside preflight, which only runs under")
	line(b, "# --check — a real run knew its borg version and had nowhere to put")
	line(b, "# it. Empty when borg is missing; preflight reports that.")
	line(b, `BORG_VERSION="$(borg --version 2>/dev/null | awk '{print $2}')"`)
	base := fileBase(job, opts)
	line(b, "LOG_FILE=%s", q(path.Join(logDirOr(opts), base+".log")))
	line(b, "LOCK_FILE=%s", q(path.Join(lockDirOr(opts), base+".lock")))
	blank(b)
	line(b, "# --check and --dry-run write to their own log. Without this, investigating")
	line(b, "# a failed real run by simply running --check truncates the very log you")
	line(b, "# are trying to read, before you get to see it.")
	line(b, `if [ "$MODE" != "real" ]; then`)
	line(b, `    LOG_FILE="${LOG_FILE}.${MODE}"`)
	line(b, "fi")
	blank(b)

	line(b, "export BORG_REPO=%s", q(job.Repo.Path))
	if job.Repo.AllowUnknownUnencrypted {
		line(b, "export BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes")
	}
	switch job.Repo.SecretMode {
	case model.SecretPassphrase:
		line(b, "export BORG_PASSPHRASE=%s", q(job.Repo.SecretValue))
	case model.SecretPasscommand:
		line(b, "export BORG_PASSCOMMAND=%s", q(job.Repo.SecretValue))
	}
	blank(b)

	switch job.Transport.Kind {
	case model.TransportSSHFS:
		line(b, "SSHFS_REMOTE=%s", q(job.Transport.SSHFSRemote))
		line(b, "SSHFS_MOUNT=%s", q(job.Transport.SSHFSMount))
		line(b, "SSHFS_OPTS=%s", q(strings.Join(job.Transport.SSHFSOpts, ",")))
		if job.Transport.SSHKey != "" {
			line(b, "SSH_KEY=%s", q(job.Transport.SSHKey))
		}
		blank(b)
	case model.TransportCIFS:
		line(b, "CIFS_SHARE=%s", q(job.Transport.CIFSShare))
		line(b, "CIFS_MOUNT=%s", q(job.Transport.CIFSMount))
		line(b, "CIFS_OPTS=%s", q(strings.Join(job.Transport.CIFSOpts, ",")))
		blank(b)
	}

	if job.Notify.Telegram.Enabled {
		line(b, "TELEGRAM_BOT=%s", q(job.Notify.Telegram.Token))
		line(b, "TELEGRAM_ID=( %s )", strings.Join(qAll(job.Notify.Telegram.ChatIDs), " "))
	}
	if job.Notify.Email.Enabled {
		line(b, "EMAIL_TO=%s", q(job.Notify.Email.To))
		line(b, "EMAIL_FROM=%s", q(job.Notify.Email.From))
		line(b, "SMTP_SERVER=%s", q(job.Notify.Email.SMTPServer))
		line(b, "SMTP_PORT=%s", q(strconv.Itoa(job.Notify.Email.SMTPPort)))
		// Credentials are optional: an internal relay often accepts mail from
		// the local network without authentication.
		if job.Notify.Email.SMTPUser != "" {
			line(b, "SMTP_USER=%s", q(job.Notify.Email.SMTPUser))
			line(b, "SMTP_PASSWORD=%s", q(job.Notify.Email.SMTPPass))
		}
	}
	if job.Notify.Webhook.Enabled {
		line(b, "PUSH_URL=%s", q(job.Notify.Webhook.PushURL))
	}
	if job.Notify.Telegram.Enabled || job.Notify.Email.Enabled || job.Notify.Webhook.Enabled {
		blank(b)
	}

	// Emitted only when something uses it. A job with no channels and no
	// monitoring used to carry this array with nothing to consume it — dead
	// config, and the same leak adding borgmon would have made worse.
	if usesCurl(job) {
		line(b, "# Every curl call is time boxed: a notification during host shutdown")
		line(b, "# must not hang until systemd escalates to SIGKILL.")
		line(b, "CURL_OPTS=( --silent --show-error --connect-timeout %d --max-time %d )",
			job.Notify.CurlConnectTimeout, job.Notify.CurlMaxTime)
		blank(b)
	}

	if m := job.Monitor; m != nil && m.Enabled {
		line(b, "# borgmon: telemetry only. Every call below is `|| true` — a monitor")
		line(b, "# that can fail the backup it watches is worse than no monitor.")
		line(b, "BORGMON_URL=%s", q(monitorURL(opts)))
		line(b, "BORGMON_TOKEN=%s", q(m.Token))
		line(b, "# Correlates the start and finish of this run. Generated here")
		line(b, "# because the scripts have no JSON parser to read an id back out")
		line(b, "# of a response.")
		line(b, `RUN_ID="${BACKUPNAME}-$(date +%%s)-$$"`)
		line(b, `BORGMON_STATS="$(mktemp -t borgmon-stats.XXXXXX)"`)
		blank(b)
	}
	line(b, "MOUNTED_BY_US=0")
	line(b, `FAIL_REASON=""`)
	line(b, "# Set to 1 only once this run owns LOG_FILE (lock taken, tee installed).")
	line(b, "# A run that never got that far must not read or attach the log: it")
	line(b, "# belongs to whichever run holds the lock.")
	line(b, "LOG_OWNED=0")
	line(b, "START_TIME=$(date +%%s)")
	blank(b)
	return nil
}

func sectionLogging(b *strings.Builder, job model.BackupJob, _ Options) error {
	header(b, "Logging")
	line(b, "# One log file per job, truncated on every run — which is why this runs")
	line(b, "# only after the lock is held: an overlapping run must never truncate the")
	line(b, "# log of the run already in progress.")
	line(b, "#")
	line(b, "# An unwritable log is a hard failure, not a warning: the log is what")
	line(b, "# gets attached to the failure notification, so losing it silently means")
	line(b, "# finding out about a broken backup with no diagnostics.")
	line(b, `LOG_DIR="$(dirname "$LOG_FILE")"`)
	line(b, `if ! mkdir -p "$LOG_DIR" 2>/dev/null || ! : >>"$LOG_FILE" 2>/dev/null; then`)
	line(b, `    FAIL_REASON="log file not writable: $LOG_FILE"`)
	line(b, "    exit 2")
	line(b, "fi")
	line(b, `exec > >(tee "$LOG_FILE") 2>&1`)
	line(b, "LOG_OWNED=1")
	blank(b)
	return nil
}

func sectionSources(b *strings.Builder, job model.BackupJob, _ Options) error {
	header(b, "Sources")
	line(b, "SOURCES=(")
	for _, p := range job.Source.Paths {
		line(b, "    %s", anchor(job, p))
	}
	line(b, ")")
	blank(b)
	return nil
}

func sectionHelpers(b *strings.Builder, job model.BackupJob, _ Options) error {
	header(b, "Helpers")
	line(b, `info() { printf '\n%%s %%s\n\n' "$(date)" "$*"; }`)
	blank(b)
	if job.Transport.Kind != model.TransportLocal {
		line(b, "# Answered from the kernel's own table rather than by touching the")
		line(b, "# path. mountpoint(1) and stat() block for ever on a mount whose")
		line(b, "# daemon has died, and this is called from on_exit, where blocking")
		line(b, "# means no notification is ever sent.")
		line(b, "is_mounted() {")
		line(b, `    awk -v p="$1" '$2 == p { found = 1 } END { exit !found }' /proc/self/mounts`)
		line(b, "}")
		blank(b)
	}
	line(b, "# Highest return code wins, as in the hand-written scripts.")
	line(b, "max_rc() {")
	line(b, "    local m=0 v")
	line(b, `    for v in "$@"; do`)
	line(b, `        if [ "$v" -gt "$m" ]; then m="$v"; fi`)
	line(b, "    done")
	line(b, `    printf '%%s' "$m"`)
	line(b, "}")
	blank(b)
	return nil
}

func sectionNotify(b *strings.Builder, job model.BackupJob, _ Options) error {
	return notifyFuncs(b, job, true)
}

// notifyFuncs emits the notify_telegram/notify_email/notify_webhook function
// definitions. includeWebhook is false for companion scripts (check,
// restore-drill): they never declare a PUSH_URL, so a notify_webhook()
// referencing it would be dead code with a dangling variable reference, not
// just an unused function.
func notifyFuncs(b *strings.Builder, job model.BackupJob, includeWebhook bool) error {
	n := job.Notify
	webhook := includeWebhook && n.Webhook.Enabled
	if !n.Telegram.Enabled && !n.Email.Enabled && !webhook {
		return nil
	}
	header(b, "Notifications")

	if n.Telegram.Enabled {
		line(b, "# Returns non-zero if any chat failed, so preflight's channel test can")
		line(b, "# actually detect a broken token. Ending a branch on `curl || info ...`")
		line(b, "# would make info's own (always zero) status the function's result.")
		line(b, "notify_telegram() {")
		line(b, `    local text="$1" attach="$2" u rc=0`)
		line(b, `    for u in "${TELEGRAM_ID[@]}"; do`)
		line(b, `        if [ "$attach" = "true" ] && [ -f "$LOG_FILE" ]; then`)
		line(b, "            # sendDocument, not sendMessage: Telegram has no way to attach a")
		line(b, "            # file to a plain text message. Its caption is capped at 1024")
		line(b, "            # characters (vs. 4096 for a text message), well above what this")
		line(b, "            # body ever contains.")
		line(b, `            curl "${CURL_OPTS[@]}" -X POST \`)
		line(b, `                "https://api.telegram.org/bot${TELEGRAM_BOT}/sendDocument" \`)
		line(b, `                -F chat_id="$u" -F caption="$text" -F document=@"$LOG_FILE" \`)
		line(b, `                >/dev/null \`)
		line(b, `                || { info "Telegram notification failed for chat $u"; rc=1; }`)
		line(b, "        else")
		line(b, `            curl "${CURL_OPTS[@]}" -X POST \`)
		line(b, `                "https://api.telegram.org/bot${TELEGRAM_BOT}/sendMessage" \`)
		line(b, `                -d chat_id="$u" -d text="$text" >/dev/null \`)
		line(b, `                || { info "Telegram notification failed for chat $u"; rc=1; }`)
		line(b, "        fi")
		line(b, "    done")
		line(b, "    return $rc")
		line(b, "}")
		blank(b)
	}

	if n.Email.Enabled {
		line(b, "notify_email() {")
		line(b, `    local subject="$1" body="$2" attach="$3"`)
		line(b, `    local boundary="borggen-$$-$(date +%%s)"`)
		line(b, "    {")
		line(b, `        printf 'From: %%s\n' "$EMAIL_FROM"`)
		line(b, `        printf 'To: %%s\n' "$EMAIL_TO"`)
		line(b, `        printf 'Subject: %%s\n' "$subject"`)
		line(b, `        printf 'MIME-Version: 1.0\n'`)
		line(b, `        printf 'Content-Type: multipart/mixed; boundary="%%s"\n\n' "$boundary"`)
		line(b, `        printf -- '--%%s\n' "$boundary"`)
		line(b, `        printf 'Content-Type: text/plain; charset=utf-8\n\n'`)
		line(b, `        printf '%%s\n' "$body"`)
		line(b, `        if [ "$attach" = "true" ] && [ -f "$LOG_FILE" ]; then`)
		line(b, `            printf -- '\n--%%s\n' "$boundary"`)
		line(b, `            printf 'Content-Type: text/plain; charset=utf-8; name="%%s.log"\n' "$BACKUPNAME"`)
		line(b, `            printf 'Content-Disposition: attachment; filename="%%s.log"\n\n' "$BACKUPNAME"`)
		line(b, `            cat "$LOG_FILE"`)
		line(b, "        fi")
		line(b, `        printf -- '\n--%%s--\n' "$boundary"`)
		line(b, `    } | curl "${CURL_OPTS[@]}" --ssl-reqd \`)
		line(b, `        --url "smtps://${SMTP_SERVER}:${SMTP_PORT}" \`)
		line(b, `        --mail-from "$EMAIL_FROM" --mail-rcpt "$EMAIL_TO" \`)
		if job.Notify.Email.SMTPUser != "" {
			line(b, `        --user "${SMTP_USER}:${SMTP_PASSWORD}" \`)
		}
		line(b, `        --upload-file - >/dev/null \`)
		line(b, `        || { info "Email notification failed"; return 1; }`)
		line(b, "}")
		blank(b)
	}

	if webhook {
		line(b, "# Heartbeat for the external dead-man's switch. Called only from a real")
		line(b, "# run: a test run must not feed it, or a failed backup stays hidden.")
		line(b, "notify_webhook() {")
		line(b, `    local status="$1" msg="$2"`)
		line(b, `    curl "${CURL_OPTS[@]}" -o /dev/null --get \`)
		line(b, `        --data-urlencode "status=$status" \`)
		line(b, `        --data-urlencode "msg=$msg" \`)
		line(b, `        "$PUSH_URL" || { info "Webhook push failed"; return 1; }`)
		line(b, "}")
		blank(b)
	}
	return nil
}

// emitAttachFlag computes the per-channel attach flag notify_telegram/
// notify_email take — AttachLogMode is a per-channel setting (Telegram and
// Email commonly want different rules), so each channel gets its own local
// variable rather than one shared between both.
//
// Every mode is additionally gated on LOG_OWNED: a run that never took the
// lock must not attach the log, because that file is the *other* run's,
// still being written. "$LOG_FILE exists" is not enough to prove ownership.
func emitAttachFlag(b *strings.Builder, varName, mode string) {
	line(b, "    local %s=false", varName)
	switch mode {
	case model.AttachAlways:
		line(b, `    if [ "$LOG_OWNED" -eq 1 ]; then %s=true; fi`, varName)
	case model.AttachOnError:
		line(b, `    if [ "$LOG_OWNED" -eq 1 ] && [ "$rc" -ge 1 ]; then %s=true; fi`, varName)
	}
}

// emitGatedNotify emits a channel's notify call, optionally skipping a real
// run's success notification (OnlyOnProblem) — never during --check/
// --dry-run, which exist specifically to prove the channel works and must
// always fire regardless of rc.
func emitGatedNotify(b *strings.Builder, onlyOnProblem bool, call string) {
	if !onlyOnProblem {
		line(b, "    %s", call)
		return
	}
	line(b, `    if [ "$MODE" != "real" ] || [ "$rc" -ne 0 ]; then`)
	line(b, "        %s", call)
	line(b, "    fi")
}

func sectionCleanupExit(b *strings.Builder, job model.BackupJob, _ Options) error {
	header(b, "Cleanup and the single notification point")

	line(b, "cleanup_mount() {")
	switch {
	case job.Transport.Kind == model.TransportSSHFS && job.Transport.UnmountOnExit:
		line(b, "    # is_mounted, not mountpoint: this runs inside on_exit, and a")
		line(b, "    # stat() on a dead mount blocks for ever — taking the failure")
		line(b, "    # notification and the monitoring push down with it, exactly")
		line(b, "    # when they matter most. -uz detaches a mount whose daemon is")
		line(b, `    # gone; plain -u answers "device is busy" and gives up.`)
		line(b, `    if [ "$MOUNTED_BY_US" -eq 1 ] && is_mounted "$SSHFS_MOUNT"; then`)
		line(b, `        fusermount -uz "$SSHFS_MOUNT" || info "Unmount failed: $SSHFS_MOUNT"`)
		line(b, "    fi")
	case job.Transport.Kind == model.TransportCIFS && job.Transport.UnmountOnExit:
		line(b, `    if [ "$MOUNTED_BY_US" -eq 1 ] && is_mounted "$CIFS_MOUNT"; then`)
		line(b, `        umount "$CIFS_MOUNT" || umount -l "$CIFS_MOUNT" || info "Unmount failed: $CIFS_MOUNT"`)
		line(b, "    fi")
	case job.Transport.Kind == model.TransportCIFS:
		line(b, "    # The share may serve other consumers, so it is left mounted.")
		line(b, "    :")
	default:
		line(b, "    :")
	}
	line(b, "}")
	blank(b)

	line(b, "# Every exit path lands here: success, warning, error and signals alike.")
	line(b, "# Classification and notification exist in exactly one place.")
	line(b, "on_exit() {")
	line(b, "    local rc=$?")
	line(b, "    trap - EXIT")
	line(b, "    cleanup_mount")
	blank(b)
	line(b, "    local duration emoji text prefix skipped files newdata")
	line(b, "    duration=$(( $(date +%%s) - START_TIME ))")
	line(b, `    duration=$(printf '%%dh %%dm %%ds' $((duration/3600)) $((duration%%3600/60)) $((duration%%60)))`)
	blank(b)
	line(b, "    # borg marks every item it could not read with the 'E' flag, which")
	line(b, "    # --filter AME includes. \"finished with warnings\" on its own does not")
	line(b, "    # say that data is missing, so the count belongs in the notification.")
	line(b, "    #")
	line(b, "    # Files/newdata come from the --stats block borg prints on a real create.")
	line(b, "    # The docs point programmatic consumers at --json instead of this text, but")
	line(b, "    # that needs a JSON parser (jq) the generated scripts do not otherwise")
	line(b, "    # depend on; this reads the same summary preflight's own version check")
	line(b, "    # already parses with awk. --stats is never present in --dry-run (it is")
	line(b, "    # mutually exclusive with --dry-run), so both stay empty there and in")
	line(b, "    # --check, and the corresponding body lines are simply omitted.")
	line(b, "    #")
	line(b, "    # LOG_OWNED gates this: a run that failed to take the lock would")
	line(b, "    # otherwise scrape the *other* run's in-progress log and report its")
	line(b, "    # numbers as its own.")
	line(b, "    skipped=0")
	line(b, `    files=""`)
	line(b, `    newdata=""`)
	line(b, `    if [ "$LOG_OWNED" -eq 1 ] && [ -f "$LOG_FILE" ]; then`)
	line(b, `        skipped="$(grep -c '^E ' "$LOG_FILE" 2>/dev/null || true)"`)
	line(b, `        [ -n "$skipped" ] || skipped=0`)
	line(b, `        files="$(awk -F': ' '/^Number of files:/ {print $2; exit}' "$LOG_FILE")"`)
	line(b, `        newdata="$(awk '/^This archive:/ {print $(NF-1), $NF; exit}' "$LOG_FILE")"`)
	line(b, "    fi")
	blank(b)
	line(b, `    case "$rc" in`)
	line(b, `        0)   emoji="✅"; text="finished successfully" ;;`)
	line(b, `        1)   emoji="⚠️"; text="finished with warnings" ;;`)
	line(b, `        2)   emoji="❌"; text="finished with errors" ;;`)
	line(b, `        129) emoji="❌"; text="interrupted by SIGHUP" ;;`)
	line(b, `        130) emoji="❌"; text="interrupted by SIGINT" ;;`)
	line(b, `        143) emoji="❌"; text="interrupted by SIGTERM" ;;`)
	line(b, `        *)   emoji="❌"; text="finished with unexpected code $rc" ;;`)
	line(b, "    esac")
	line(b, `    if [ -n "$FAIL_REASON" ]; then text="$text: $FAIL_REASON"; fi`)
	blank(b)
	line(b, `    prefix=""`)
	line(b, `    case "$MODE" in`)
	line(b, `        check)   prefix="[CHECK] " ;;`)
	line(b, `        dry-run) prefix="[DRY-RUN] " ;;`)
	line(b, "    esac")
	blank(b)
	line(b, `    local subject="${prefix}${emoji} $(date '+%%Y-%%m-%%d %%H:%%M') - borg - ${BACKUPNAME}"`)
	line(b, `    local body="${subject}"$'\n'"${text}"$'\n'"Duration: ${duration}"`)
	line(b, `    if [ -n "$files" ]; then body="${body}"$'\n'"Files: ${files}"; fi`)
	line(b, `    if [ -n "$newdata" ]; then body="${body}"$'\n'"New data: ${newdata}"; fi`)
	line(b, `    if [ "$skipped" -gt 0 ] 2>/dev/null; then`)
	line(b, `        body="${body}"$'\n'"Items skipped (unreadable): ${skipped}"`)
	line(b, `        info "$skipped item(s) could not be read — the archive is incomplete"`)
	line(b, "    fi")
	line(b, `    info "$text (rc=$rc, mode=$MODE, duration=$duration)"`)
	blank(b)

	if job.Notify.Telegram.Enabled {
		emitAttachFlag(b, "attach_tg", job.Notify.Telegram.AttachLogMode)
		emitGatedNotify(b, job.Notify.Telegram.OnlyOnProblem, `notify_telegram "$body" "$attach_tg" || true`)
	}
	if job.Notify.Email.Enabled {
		emitAttachFlag(b, "attach_email", job.Notify.Email.AttachLogMode)
		emitGatedNotify(b, job.Notify.Email.OnlyOnProblem, `notify_email "$subject" "$body" "$attach_email" || true`)
	}
	if job.Notify.Webhook.Enabled {
		line(b, "    # Test runs never touch the heartbeat.")
		line(b, `    if [ "$MODE" = "real" ]; then`)
		line(b, `        if [ "$rc" -le 1 ]; then`)
		line(b, `            notify_webhook "up" "$text" || true`)
		if job.Notify.Webhook.NotifyOnFailure {
			line(b, "        else")
			line(b, `            notify_webhook "down" "$text" || true`)
		}
		line(b, "        fi")
		line(b, "    fi")
	}
	blank(b)
	emitMonitorFinish(b, job)
	line(b, `    exit "$rc"`)
	line(b, "}")
	blank(b)
	return nil
}

func sectionTraps(b *strings.Builder, _ model.BackupJob, _ Options) error {
	header(b, "Traps")
	line(b, "# Signal traps exist to turn death-by-signal into a normal exit: without")
	line(b, "# them bash dies from the signal and the EXIT trap never runs. Each one")
	line(b, "# disarms the others first, so a second Ctrl+C cannot re-enter.")
	line(b, `trap 'trap - INT TERM HUP; exit 129' HUP`)
	line(b, `trap 'trap - INT TERM HUP; exit 130' INT`)
	line(b, `trap 'trap - INT TERM HUP; exit 143' TERM`)
	line(b, "trap on_exit EXIT")
	blank(b)
	return nil
}

func sectionLock(b *strings.Builder, _ model.BackupJob, _ Options) error {
	header(b, "Single instance")
	line(b, `mkdir -p "$(dirname "$LOCK_FILE")" 2>/dev/null || true`)
	line(b, `if ! exec 9>"$LOCK_FILE"; then`)
	line(b, `    FAIL_REASON="cannot open lock file $LOCK_FILE"`)
	line(b, "    exit 2")
	line(b, "fi")
	line(b, "if ! flock -n 9; then")
	line(b, `    FAIL_REASON="another run is already in progress"`)
	line(b, "    exit 1")
	line(b, "fi")
	blank(b)
	return nil
}

func sectionMount(b *strings.Builder, job model.BackupJob, _ Options) error {
	switch job.Transport.Kind {
	case model.TransportSSHFS:
		header(b, "Mount source (sshfs, pull backup)")
		line(b, `mkdir -p "$SSHFS_MOUNT"`)
		line(b, "# 9>&- keeps the lock descriptor out of the mount daemon. sshfs")
		line(b, "# detaches and outlives this script, and flock lives on the")
		line(b, "# descriptor rather than the file — so an sshfs that survives a")
		line(b, "# failed unmount holds the lock for ever, and every later run")
		line(b, `# exits with "another run is already in progress". Seen in`)
		line(b, "# production: a job silently stopped running and looked healthy.")
		line(b, `if is_mounted "$SSHFS_MOUNT"; then`)
		line(b, `    info "sshfs already mounted at $SSHFS_MOUNT"`)
		line(b, "else")
		if job.Transport.SSHKey != "" {
			line(b, `    if ! sshfs -o "$SSHFS_OPTS" -o IdentityFile="$SSH_KEY" "$SSHFS_REMOTE" "$SSHFS_MOUNT" 9>&-; then`)
		} else {
			line(b, `    if ! sshfs -o "$SSHFS_OPTS" "$SSHFS_REMOTE" "$SSHFS_MOUNT" 9>&-; then`)
		}
		line(b, `        FAIL_REASON="sshfs mount failed: $SSHFS_REMOTE"`)
		line(b, "        exit 2")
		line(b, "    fi")
		line(b, "    MOUNTED_BY_US=1")
		line(b, "fi")
		blank(b)
		line(b, "# Mounted is not the same as usable. The timeout bounds a slow")
		line(b, "# mount, not a wedged one: a process blocked in an uninterruptible")
		line(b, "# FUSE wait ignores signals, so nothing inside this script can")
		line(b, "# rescue that case — only aborting the connection can.")
		line(b, `if ! is_mounted "$SSHFS_MOUNT" || ! timeout 10 ls "$SSHFS_MOUNT" >/dev/null 2>&1; then`)
		line(b, `    FAIL_REASON="mount point not accessible: $SSHFS_MOUNT"`)
		line(b, "    exit 2")
		line(b, "fi")
		blank(b)
	case model.TransportCIFS:
		header(b, "Mount source (cifs)")
		line(b, `mkdir -p "$CIFS_MOUNT"`)
		line(b, `if is_mounted "$CIFS_MOUNT"; then`)
		line(b, `    info "cifs share already mounted at $CIFS_MOUNT"`)
		line(b, "else")
		line(b, `    if ! mount -t cifs "$CIFS_SHARE" "$CIFS_MOUNT" -o "$CIFS_OPTS"; then`)
		line(b, `        FAIL_REASON="cifs mount failed: $CIFS_SHARE"`)
		line(b, "        exit 2")
		line(b, "    fi")
		line(b, "    MOUNTED_BY_US=1")
		line(b, "fi")
		blank(b)
		line(b, `if ! is_mounted "$CIFS_MOUNT" || ! timeout 10 ls "$CIFS_MOUNT" >/dev/null 2>&1; then`)
		line(b, `    FAIL_REASON="mount point not accessible: $CIFS_MOUNT"`)
		line(b, "    exit 2")
		line(b, "fi")
		blank(b)
	}
	return nil
}

func sectionPreflight(b *strings.Builder, job model.BackupJob, _ Options) error {
	header(b, "Preflight")
	line(b, "# Environment verification only: nothing here writes to the repository.")
	line(b, "preflight() {")
	line(b, "    local rc=0 ver mm oldest s")
	blank(b)
	line(b, `    if ! command -v borg >/dev/null 2>&1; then`)
	line(b, `        info "FAIL: borg not found in PATH"`)
	line(b, "        return 2")
	line(b, "    fi")
	line(b, `    ver="$BORG_VERSION"`)
	line(b, `    mm="$(printf '%%s' "$ver" | cut -d. -f1,2)"`)
	line(b, `    if [ -z "$mm" ]; then`)
	line(b, `        info "FAIL: cannot determine borg version"`)
	line(b, "        rc=2")
	line(b, "    else")
	line(b, `        oldest="$(printf '%%s\n%%s\n' "$BORG_TARGET" "$mm" | sort -V | head -n1)"`)
	line(b, `        if [ "$oldest" != "$BORG_TARGET" ]; then`)
	line(b, `            info "FAIL: borg $ver is older than target $BORG_TARGET"`)
	line(b, "            rc=2")
	line(b, "        else")
	line(b, `            info "OK: borg $ver (target $BORG_TARGET)"`)
	line(b, "        fi")
	line(b, "    fi")
	blank(b)
	line(b, "    if borg info >/dev/null 2>&1; then")
	line(b, `        info "OK: repository reachable: $BORG_REPO"`)
	line(b, "    else")
	line(b, `        info "FAIL: repository not reachable: $BORG_REPO"`)
	line(b, "        rc=2")
	line(b, "    fi")
	blank(b)
	line(b, `    for s in "${SOURCES[@]}"; do`)
	line(b, `        if [ -e "$s" ]; then`)
	line(b, `            info "OK: source exists: $s"`)
	line(b, "        else")
	line(b, `            info "FAIL: source missing: $s"`)
	line(b, "            rc=2")
	line(b, "        fi")
	line(b, "    done")
	blank(b)
	line(b, `    if [ -w "$(dirname "$LOG_FILE")" ]; then`)
	line(b, `        info "OK: log directory writable"`)
	line(b, "    else")
	line(b, `        info "FAIL: log directory not writable: $(dirname "$LOG_FILE")"`)
	line(b, "        rc=2")
	line(b, "    fi")
	line(b, `    if [ -w "$LOCK_FILE" ]; then`)
	line(b, `        info "OK: lock file writable: $LOCK_FILE"`)
	line(b, "    else")
	line(b, `        info "FAIL: lock file not writable: $LOCK_FILE"`)
	line(b, "        rc=2")
	line(b, "    fi")

	if job.Notify.Telegram.Enabled || job.Notify.Email.Enabled {
		blank(b)
		line(b, "    # Verify the channels while someone is watching, not during an outage.")
		line(b, "    # if/else, not `cmd && ok || fail`: in that idiom a non-zero status")
		line(b, "    # from the success branch would run the failure branch as well.")
		if job.Notify.Telegram.Enabled {
			line(b, `    if notify_telegram "[CHECK] borg - ${BACKUPNAME}: preflight test message" "false"; then`)
			line(b, `        info "OK: telegram reachable"`)
			line(b, "    else")
			line(b, `        info "FAIL: telegram"`)
			line(b, "        rc=2")
			line(b, "    fi")
		}
		if job.Notify.Email.Enabled {
			line(b, `    if notify_email "[CHECK] borg - ${BACKUPNAME}" "preflight test message" "false"; then`)
			line(b, `        info "OK: smtp reachable"`)
			line(b, "    else")
			line(b, `        info "FAIL: smtp"`)
			line(b, "        rc=2")
			line(b, "    fi")
		}
	}
	if job.Notify.Webhook.Enabled {
		blank(b)
		line(b, "    # The heartbeat is deliberately not pushed here.")
		line(b, `    info "SKIP: heartbeat push is never sent from a test run"`)
	}
	blank(b)
	line(b, "    return $rc")
	line(b, "}")
	blank(b)
	line(b, "preflight_exit=0")
	line(b, `if [ "$MODE" != "real" ]; then`)
	line(b, "    preflight || preflight_exit=$?")
	line(b, "fi")
	blank(b)
	return nil
}

func sectionUserPre(b *strings.Builder, _ model.BackupJob, opts Options) error {
	header(b, "Job-specific preparation")
	line(b, `if [ "$MODE" != "check" ]; then`)
	line(b, "%s", UserPreBegin)
	if strings.TrimSpace(opts.UserPre) != "" {
		line(b, "%s", strings.TrimRight(opts.UserPre, "\n"))
	} else {
		line(b, "    # Put commands to run before the job here.")
		line(b, "    # Preserved when the script is regenerated.")
		line(b, "    :")
	}
	line(b, "%s", UserPreEnd)
	line(b, "fi")
	blank(b)
	return nil
}

func sectionUserPost(b *strings.Builder, _ model.BackupJob, opts Options) error {
	header(b, "Job-specific cleanup")
	line(b, `if [ "$MODE" != "check" ]; then`)
	line(b, "%s", UserPostBegin)
	if strings.TrimSpace(opts.UserPost) != "" {
		line(b, "%s", strings.TrimRight(opts.UserPost, "\n"))
	} else {
		line(b, "    # Put commands to run after the job here.")
		line(b, "    # Preserved when the script is regenerated.")
		line(b, "    :")
	}
	line(b, "%s", UserPostEnd)
	line(b, "fi")
	blank(b)
	return nil
}

func sectionBorg(b *strings.Builder, job model.BackupJob, _ Options) error {
	header(b, "Borg")
	line(b, "backup_exit=0")
	line(b, "prune_exit=0")
	line(b, "compact_exit=0")
	line(b, "check_exit=0")
	blank(b)

	// create
	line(b, `if [ "$MODE" != "check" ]; then`)
	line(b, `    info "Creating archive ${ARCHIVE} (mode: $MODE)"`)
	line(b, "    CREATE_OPTS=(")
	line(b, "        --verbose")
	line(b, "        --filter AME")
	line(b, "        --list")
	line(b, "        --show-rc")
	line(b, "        --compression %s", q(job.Source.Compression))
	if job.Source.CheckpointInterval > 0 {
		line(b, "        --checkpoint-interval %d", job.Source.CheckpointInterval)
	}
	if job.Source.ExcludeCaches {
		line(b, "        --exclude-caches")
	}
	for _, tag := range job.Source.ExcludeIfPresent {
		line(b, "        --exclude-if-present %s", q(tag))
	}
	if job.Source.KeepExcludeTags {
		line(b, "        --keep-exclude-tags")
	}
	if job.Source.OneFileSystem {
		line(b, "        --one-file-system")
	}
	if job.Source.NoACLs {
		line(b, "        --noacls")
	}
	if job.Source.NoXattrs {
		line(b, "        --noxattrs")
	}
	if job.Transport.Kind == model.TransportSSHFS {
		// SFTP does not carry inode numbers, so the files cache's default
		// ctime,size,inode comparison sees every file as changed on every
		// run over sshfs — correct output, just a full re-read/re-hash of
		// the whole tree each time. ctime (not mtime) stays: mtime can be
		// set from user space, ctime cannot. Not a checkbox: like
		// --one-file-system, the right value is fully determined by
		// Transport.Kind, so there is nothing for the user to decide.
		line(b, "        --files-cache ctime,size")
	}
	if job.Repo.LockWait > 0 {
		line(b, "        --lock-wait %d", job.Repo.LockWait)
	}
	for _, e := range job.Source.Excludes {
		line(b, "        --exclude %s", anchor(job, e))
	}
	line(b, "    )")
	line(b, `    if [ "$MODE" = "dry-run" ]; then`)
	line(b, "        # --stats and --dry-run are mutually exclusive: during a dry run the")
	line(b, "        # data is neither compressed nor deduplicated, so there is nothing")
	line(b, "        # to report. Passing both aborts borg on argument parsing.")
	line(b, "        CREATE_OPTS+=( --dry-run )")
	line(b, "    else")
	line(b, "        CREATE_OPTS+=( --stats )")
	line(b, "    fi")
	line(b, `    borg create "${CREATE_OPTS[@]}" "::${ARCHIVE}" "${SOURCES[@]}"`)
	line(b, "    backup_exit=$?")
	line(b, "fi")
	blank(b)

	// prune
	if job.Retention.Prune {
		line(b, `if [ "$MODE" != "check" ]; then`)
		line(b, `    info "Pruning repository"`)
		line(b, "    # --glob-archives is derived from BACKUPNAME and is not optional: the")
		line(b, "    # repository is shared, an unfiltered prune deletes other jobs' archives.")
		line(b, "    PRUNE_OPTS=(")
		line(b, "        --list")
		line(b, "        --show-rc")
		line(b, `        --glob-archives "${BACKUPNAME}-*"`)
		for _, kv := range pruneKeeps(job.Retention) {
			line(b, "        %s", kv)
		}
		if job.Repo.LockWait > 0 {
			line(b, "        --lock-wait %d", job.Repo.LockWait)
		}
		line(b, "    )")
		line(b, `    if [ "$MODE" = "dry-run" ]; then`)
		line(b, "        PRUNE_OPTS+=( --dry-run )")
		line(b, "    fi")
		line(b, `    borg prune "${PRUNE_OPTS[@]}"`)
		line(b, "    prune_exit=$?")
		line(b, "fi")
		blank(b)
	}

	// compact
	if job.Retention.Compact {
		line(b, `if [ "$MODE" = "real" ]; then`)
		line(b, `    info "Compacting repository"`)
		if job.Retention.CompactThreshold > 0 {
			line(b, "    borg compact --threshold %d", job.Retention.CompactThreshold)
		} else {
			line(b, "    borg compact")
		}
		line(b, "    compact_exit=$?")
		line(b, "fi")
		blank(b)
	}

	// check
	if job.Verify.Enabled {
		line(b, `if [ "$MODE" != "check" ]; then`)
		line(b, `    info "Checking repository (partial, time boxed)"`)
		line(b, "    # --max-duration requires --repository-only and splits a long check")
		line(b, "    # into partial runs; a full check belongs in a separate job.")
		line(b, "    borg check --repository-only --max-duration %d", job.Verify.MaxDuration)
		line(b, "    check_exit=$?")
		line(b, "fi")
		blank(b)
	}
	emitMonitorStats(b, job)
	return nil
}

func sectionFinish(b *strings.Builder, _ model.BackupJob, _ Options) error {
	header(b, "Result")
	line(b, `if [ "$MODE" = "check" ]; then`)
	line(b, "    global_exit=$preflight_exit")
	line(b, "else")
	line(b, `    global_exit=$(max_rc "$preflight_exit" "$backup_exit" "$prune_exit" "$compact_exit" "$check_exit")`)
	line(b, "fi")
	blank(b)
	line(b, "# on_exit (EXIT trap) does the classification and the notifications.")
	line(b, `exit "$global_exit"`)
	return nil
}

// --- helpers ----------------------------------------------------------------

func pruneKeeps(r model.Retention) []string {
	var out []string
	add := func(flag string, v int) {
		if v > 0 {
			out = append(out, fmt.Sprintf("%s %d", flag, v))
		}
	}
	if r.KeepWithin != "" {
		out = append(out, "--keep-within "+q(r.KeepWithin))
	}
	add("--keep-last", r.KeepLast)
	add("--keep-minutely", r.KeepMinutely)
	add("--keep-hourly", r.KeepHourly)
	add("--keep-daily", r.KeepDaily)
	add("--keep-weekly", r.KeepWeekly)
	add("--keep-monthly", r.KeepMonthly)
	add("--keep-yearly", r.KeepYearly)
	add("--keep-13weekly", r.Keep13Weekly)
	add("--keep-3monthly", r.Keep3Monthly)
	return out
}

// mountVar is the shell variable holding the mount point, empty for local.
func mountVar(job model.BackupJob) string {
	switch job.Transport.Kind {
	case model.TransportSSHFS:
		return "SSHFS_MOUNT"
	case model.TransportCIFS:
		return "CIFS_MOUNT"
	}
	return ""
}

// anchor renders a source-side path as a ready-to-use, double-quoted bash
// token anchored to the mount point. Unanchored patterns get the prefix too:
// without it they silently match outside the backed-up subtree.
func anchor(job model.BackupJob, p string) string {
	v := mountVar(job)
	if v == "" {
		return `"` + shEscape(p) + `"`
	}
	prefix := "${" + v + "}"
	switch {
	case p == "/" || p == "":
		return `"` + prefix + `"`
	case strings.HasPrefix(p, "/"):
		return `"` + prefix + shEscape(p) + `"`
	default:
		return `"` + prefix + "/" + shEscape(p) + `"`
	}
}

// shEscape neutralises everything that could break out of double quotes.
func shEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`).Replace(s)
}

// line writes one formatted line. Percent signs in literal bash must be
// doubled by the caller.
func line(b *strings.Builder, format string, args ...any) {
	fmt.Fprintf(b, format, args...)
	b.WriteByte('\n')
}

func blank(b *strings.Builder) { b.WriteByte('\n') }

func header(b *strings.Builder, title string) {
	const width = 78
	dashes := width - len("# --- ") - len(title) - 1
	if dashes < 3 {
		dashes = 3
	}
	line(b, "# --- %s %s", title, strings.Repeat("-", dashes))
}

// q single-quotes a value for bash.
func q(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func qAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = q(s)
	}
	return out
}

var crlf = regexp.MustCompile(`\r\n?`)

func normalizeLF(s string) string { return crlf.ReplaceAllString(s, "\n") }

// StripTimestamp removes the generation timestamp line so two scripts can be
// compared for real differences. Used by the idempotency test and by the
// library save path, which must not commit a timestamp-only change.
func StripTimestamp(script string) string {
	lines := strings.Split(script, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(l, timestampPrefix) {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// ExtractUserZones returns the hand-written blocks of an existing script so
// they can be carried into a regenerated one.
func ExtractUserZones(script string) (pre, post string) {
	return between(script, UserPreBegin, UserPreEnd), between(script, UserPostBegin, UserPostEnd)
}

func between(s, begin, end string) string {
	i := strings.Index(s, begin)
	if i < 0 {
		return ""
	}
	i += len(begin)
	j := strings.Index(s[i:], end)
	if j < 0 {
		return ""
	}
	return strings.Trim(s[i:i+j], "\n")
}
