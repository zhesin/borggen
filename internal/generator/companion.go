// Companion scripts are small, standalone siblings of the main backup
// script: a monthly full repository check and a restore drill. Both reuse
// the job's Repo and Notify blocks only — Transport and Source describe how
// to reach the *data*, which neither script touches, and Retention describes
// pruning, which is exclusively the main script's concern.
//
// Unlike the main script, a companion script carries no borggen:meta block:
// it is a byproduct of the current job, not something meant to be
// hand-edited and re-imported. Regenerate it from the form instead.
package generator

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"borggen/internal/model"
)

// Companion script filename suffixes: <name>-check.sh, <name>-restore-drill.sh.
// Exported so httpapi can build the same filenames when saving to the
// library and filter them back out of a job-picker listing — companion
// scripts carry no meta block, so they are not something to load into the
// form the way a real job file is.
const (
	CheckSuffix        = "check"
	RestoreDrillSuffix = "restore-drill"
)

// GenerateCheckScript renders a standalone full-repository consistency check:
// `borg check --verify-data`, i.e. both of borg check's steps (repository
// AND archives), with cryptographic data verification on top. --verify-data
// conflicts with --repository-only (it needs the archives step to run), so
// this must never pass both. This is deliberately not part of the main
// script — a full check reads (and --verify-data decrypts) every chunk in
// the repository, far too slow for a nightly job, which is why the main
// script's own Verify step is time-boxed with --repository-only and skips
// --verify-data entirely. This one is meant for a rare cron entry instead.
func GenerateCheckScript(job model.BackupJob, opts Options) (string, error) {
	job.Normalize()
	if problems := job.Validate(); len(problems) > 0 {
		return "", fmt.Errorf("invalid parameters: %s", joinProblems(problems))
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}

	var b strings.Builder
	logPath, lockPath := companionPaths(job, opts, CheckSuffix)

	companionHeader(&b, job, opts, CheckSuffix,
		[]string{
			"# Full repository check: borg check --verify-data.",
			"# Slow and unbounded on purpose — the main job's own Verify step exists",
			"# so this heavier one does not have to run nightly.",
		},
		fmt.Sprintf("0 3 1 * * %s-%s.sh", fileBase(job, opts), CheckSuffix))
	companionConfig(&b, job, opts, CheckSuffix, logPath, lockPath)
	if err := sectionHelpers(&b, job, opts); err != nil {
		return "", err
	}
	if err := notifyFuncs(&b, job, false); err != nil {
		return "", err
	}
	companionCleanupExit(&b, job, "borg check")
	if err := sectionTraps(&b, job, opts); err != nil {
		return "", err
	}
	// Lock before logging: the logging setup truncates LOG_FILE, so an
	// overlapping run must not reach it before failing to take the lock.
	// Same ordering rule as the main script (see Generate).
	if err := sectionLock(&b, job, opts); err != nil {
		return "", err
	}
	if err := sectionLogging(&b, job, opts); err != nil {
		return "", err
	}
	companionPreflight(&b, job)

	header(&b, "Check")
	line(&b, "check_exit=0")
	line(&b, `if [ "$preflight_exit" -ne 0 ]; then`)
	line(&b, "    check_exit=$preflight_exit")
	line(&b, "else")
	line(&b, `    info "Running full repository check (--verify-data reads and decrypts everything — this can take a long time)"`)
	line(&b, "    borg check --verify-data")
	line(&b, "    check_exit=$?")
	line(&b, "fi")
	blank(&b)
	line(&b, `exit "$check_exit"`)

	return normalizeLF(b.String()), nil
}

// GenerateRestoreDrillScript renders a standalone restore drill: extracts the
// most recent archive with --dry-run. The borg documentation names this as
// the best check that a backup actually works — "the best check that
// everything is ok is to run a dry-run extraction" — which is a check of the
// *data*, not of this script, and belongs in its own rarely-run job rather
// than the nightly run modes.
func GenerateRestoreDrillScript(job model.BackupJob, opts Options) (string, error) {
	job.Normalize()
	if problems := job.Validate(); len(problems) > 0 {
		return "", fmt.Errorf("invalid parameters: %s", joinProblems(problems))
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}

	var b strings.Builder
	logPath, lockPath := companionPaths(job, opts, RestoreDrillSuffix)

	companionHeader(&b, job, opts, RestoreDrillSuffix,
		[]string{
			"# Restore drill: dry-run extraction of the most recent archive.",
			"# Writes nothing to disk (--dry-run) and never touches the repository —",
			"# safe to run any time. Confirms the backup can actually be read back,",
			"# not just that borg create exited 0.",
		},
		fmt.Sprintf("0 4 1 */3 * %s-%s.sh", fileBase(job, opts), RestoreDrillSuffix))
	companionConfig(&b, job, opts, RestoreDrillSuffix, logPath, lockPath)
	if err := sectionHelpers(&b, job, opts); err != nil {
		return "", err
	}
	if err := notifyFuncs(&b, job, false); err != nil {
		return "", err
	}
	companionCleanupExit(&b, job, "borg extract --dry-run")
	if err := sectionTraps(&b, job, opts); err != nil {
		return "", err
	}
	// Lock before logging: the logging setup truncates LOG_FILE, so an
	// overlapping run must not reach it before failing to take the lock.
	// Same ordering rule as the main script (see Generate).
	if err := sectionLock(&b, job, opts); err != nil {
		return "", err
	}
	if err := sectionLogging(&b, job, opts); err != nil {
		return "", err
	}
	companionPreflight(&b, job)

	header(&b, "Restore drill")
	line(&b, "drill_exit=0")
	line(&b, `if [ "$preflight_exit" -ne 0 ]; then`)
	line(&b, "    drill_exit=$preflight_exit")
	line(&b, "else")
	line(&b, `    LATEST="$(borg list --last 1 --short "$BORG_REPO" 2>/dev/null)"`)
	line(&b, `    if [ -z "$LATEST" ]; then`)
	line(&b, `        info "FAIL: no archives found in the repository"`)
	line(&b, "        drill_exit=2")
	line(&b, "    else")
	line(&b, `        info "Dry-run extracting latest archive: $LATEST"`)
	line(&b, `        borg extract --dry-run --list "${BORG_REPO}::${LATEST}"`)
	line(&b, "        drill_exit=$?")
	line(&b, "    fi")
	line(&b, "fi")
	blank(&b)
	line(&b, `exit "$drill_exit"`)

	return normalizeLF(b.String()), nil
}

// companionPaths derives the check/restore-drill script's own log and lock
// paths, same fixed directories as the main script (/var/log/borg,
// /var/lock/borg — neither is user-overridable, so there is no per-job
// directory to inherit here). The filename part follows opts.Filename (the
// on-disk name the main script is saved/pushed as), falling back to the
// job's BACKUPNAME when Filename is not given.
func companionPaths(job model.BackupJob, opts Options, suffix string) (logPath, lockPath string) {
	base := fileBase(job, opts)
	logPath = path.Join(logDirOr(opts), base+"-"+suffix+".log")
	lockPath = path.Join(lockDirOr(opts), base+"-"+suffix+".lock")
	return logPath, lockPath
}

func companionHeader(b *strings.Builder, job model.BackupJob, opts Options, kind string, purpose []string, cronExample string) {
	line(b, "#!/bin/bash")
	line(b, "#")
	line(b, "# Companion %s job for: %s", kind, job.Name)
	line(b, "# Generated by borggen %s", Version)
	line(b, "%s%s", timestampPrefix, opts.Now.UTC().Format(time.RFC3339))
	line(b, "# Target borg version: %s", job.BorgTarget)
	line(b, "#")
	for _, l := range purpose {
		line(b, "%s", l)
	}
	line(b, "#")
	line(b, "# Not round-tripped: this file carries no borggen:meta block. Re-generate it")
	line(b, "# from the main job in borggen instead of hand-editing it.")
	line(b, "#")
	line(b, "# Suggested cron: %s", cronExample)
	blank(b)
}

// companionConfig is a trimmed sectionConfig: no MODE (companion scripts take
// no arguments), no Transport (neither script reads Source), no Retention.
func companionConfig(b *strings.Builder, job model.BackupJob, _ Options, _ string, logPath, lockPath string) {
	header(b, "Config")
	line(b, "BACKUPNAME=%s", q(job.Name))
	line(b, "BORG_TARGET=%s", q(job.BorgTarget))
	line(b, "LOG_FILE=%s", q(logPath))
	line(b, "LOCK_FILE=%s", q(lockPath))
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

	if job.Notify.Telegram.Enabled {
		line(b, "TELEGRAM_BOT=%s", q(job.Notify.Telegram.Token))
		line(b, "TELEGRAM_ID=( %s )", strings.Join(qAll(job.Notify.Telegram.ChatIDs), " "))
	}
	if job.Notify.Email.Enabled {
		line(b, "EMAIL_TO=%s", q(job.Notify.Email.To))
		line(b, "EMAIL_FROM=%s", q(job.Notify.Email.From))
		line(b, "SMTP_SERVER=%s", q(job.Notify.Email.SMTPServer))
		line(b, "SMTP_PORT=%s", q(strconv.Itoa(job.Notify.Email.SMTPPort)))
		if job.Notify.Email.SMTPUser != "" {
			line(b, "SMTP_USER=%s", q(job.Notify.Email.SMTPUser))
			line(b, "SMTP_PASSWORD=%s", q(job.Notify.Email.SMTPPass))
		}
	}
	// No PUSH_URL: the webhook heartbeat belongs to the main job alone — a
	// monthly check or an ad-hoc drill feeding the same dead-man's switch
	// would just be a second, confusing source of truth for "is it alive".
	if job.Notify.Telegram.Enabled || job.Notify.Email.Enabled {
		blank(b)
	}

	line(b, "# Every curl call is time boxed: a notification during host shutdown")
	line(b, "# must not hang until systemd escalates to SIGKILL.")
	line(b, "CURL_OPTS=( --silent --show-error --connect-timeout %d --max-time %d )",
		job.Notify.CurlConnectTimeout, job.Notify.CurlMaxTime)
	blank(b)
	line(b, `FAIL_REASON=""`)
	line(b, "# Set to 1 only once this run owns LOG_FILE (lock taken, tee installed).")
	line(b, "# A run that never got that far must not attach the log: it belongs to")
	line(b, "# whichever run holds the lock.")
	line(b, "LOG_OWNED=0")
	line(b, "START_TIME=$(date +%%s)")
	blank(b)
}

// companionCleanupExit is a trimmed sectionCleanupExit: no cleanup_mount (no
// transport to unmount), no MODE (so no [CHECK]/[DRY-RUN] prefix and no
// create --stats-derived Files/New data — those come from `borg create`
// alone), no webhook call.
func companionCleanupExit(b *strings.Builder, job model.BackupJob, label string) {
	header(b, "The single notification point")
	line(b, "# Every exit path lands here: success, warning, error and signals alike.")
	line(b, "on_exit() {")
	line(b, "    local rc=$?")
	line(b, "    trap - EXIT")
	blank(b)
	line(b, "    local duration emoji text subject body")
	line(b, "    duration=$(( $(date +%%s) - START_TIME ))")
	line(b, `    duration=$(printf '%%dh %%dm %%ds' $((duration/3600)) $((duration%%3600/60)) $((duration%%60)))`)
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
	line(b, `    subject="${emoji} $(date '+%%Y-%%m-%%d %%H:%%M') - %s - ${BACKUPNAME}"`, label)
	line(b, `    body="${subject}"$'\n'"${text}"$'\n'"Duration: ${duration}"`)
	line(b, `    info "$text (rc=$rc, duration=$duration)"`)
	blank(b)

	// Companion scripts have no run modes to gate on, so OnlyOnProblem is
	// never applied here — always notify, same as before that setting existed.
	if job.Notify.Telegram.Enabled {
		emitAttachFlag(b, "attach_tg", job.Notify.Telegram.AttachLogMode)
		line(b, `    notify_telegram "$body" "$attach_tg" || true`)
	}
	if job.Notify.Email.Enabled {
		emitAttachFlag(b, "attach_email", job.Notify.Email.AttachLogMode)
		line(b, `    notify_email "$subject" "$body" "$attach_email" || true`)
	}
	blank(b)
	line(b, `    exit "$rc"`)
	line(b, "}")
	blank(b)
}

// companionPreflight is a trimmed sectionPreflight: no SOURCES loop (neither
// script reads Source), always runs (no MODE to gate on — a companion script
// takes no arguments).
func companionPreflight(b *strings.Builder, job model.BackupJob) {
	header(b, "Preflight")
	line(b, "# Environment verification only: nothing here writes to the repository.")
	line(b, "preflight() {")
	line(b, "    local rc=0 ver mm oldest")
	blank(b)
	line(b, `    if ! command -v borg >/dev/null 2>&1; then`)
	line(b, `        info "FAIL: borg not found in PATH"`)
	line(b, "        return 2")
	line(b, "    fi")
	line(b, `    ver="$(borg --version 2>/dev/null | awk '{print $2}')"`)
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
	blank(b)
	line(b, "    return $rc")
	line(b, "}")
	blank(b)
	line(b, "preflight_exit=0")
	line(b, "preflight || preflight_exit=$?")
	blank(b)
	_ = job // job kept for signature symmetry with the other companion helpers
}

func joinProblems(problems []model.Problem) string {
	parts := make([]string, len(problems))
	for i, p := range problems {
		parts[i] = p.String()
	}
	return strings.Join(parts, "; ")
}
