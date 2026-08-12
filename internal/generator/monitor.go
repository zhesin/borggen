package generator

import (
	"strings"

	"borggen/internal/model"
)

// borgmon telemetry. Everything here is emitted only when Monitor is enabled:
// a job without it must produce a script byte-for-byte identical to one
// generated before the feature existed — no variables, no calls, not even a
// comment. TestNoMonitorTraceWhenDisabled pins that.
//
// Three rules the whole file follows:
//
//   - every call ends in `|| true`. A monitor that can fail the backup it
//     watches is worse than no monitor.
//   - the script never parses a response. It has no JSON parser, so the run
//     id is generated locally rather than read back from the server.
//   - the log goes in its own request. Bundled into the finish push, a
//     multi-megabyte body would sit behind that push's --max-time, and
//     blowing it would cost the run record — which matters more than the log.

// monitoring reports whether this job pushes to borgmon at all.
func monitoring(job model.BackupJob) bool {
	return job.Monitor != nil && job.Monitor.Enabled
}

// usesCurl reports whether anything in the script needs CURL_OPTS. Without
// this the array was emitted even for a job with no channels and no
// monitoring — dead config that adding a second consumer would only have
// made more confusing.
func usesCurl(job model.BackupJob) bool {
	n := job.Notify
	return n.Telegram.Enabled || n.Email.Enabled || n.Webhook.Enabled || monitoring(job)
}

// monitorURL is the borgmon base URL. Like Filename, it is a generation-time
// input rather than a job parameter: it belongs to the deployment, is the
// same for every job on the server, and has no business round-tripping
// through a script's meta block.
func monitorURL(opts Options) string {
	return strings.TrimRight(opts.MonitorURL, "/")
}

// sectionMonitorStart opens the run. It sits after the lock and the logging
// setup so that a run which lost the lock race never reports itself as
// started, and so the push itself is captured in the log.
func sectionMonitorStart(b *strings.Builder, job model.BackupJob, _ Options) error {
	if !monitoring(job) {
		return nil
	}
	header(b, "borgmon: run start")
	line(b, "# Opens the run so the dashboard can show it as running, and so a")
	line(b, "# script killed by SIGKILL leaves a record that stops at 'started'")
	line(b, "# instead of leaving no trace at all.")
	line(b, `curl "${CURL_OPTS[@]}" -X POST \`)
	line(b, `    -H "Authorization: Bearer ${BORGMON_TOKEN}" \`)
	line(b, `    --data-urlencode "event=start" \`)
	line(b, `    --data-urlencode "run_id=${RUN_ID}" \`)
	line(b, `    --data-urlencode "mode=${MODE}" \`)
	line(b, `    --data-urlencode "backupname=${BACKUPNAME}" \`)
	line(b, `    --data-urlencode "hostname=$(hostname)" \`)
	line(b, `    --data-urlencode "borg_version=${BORG_VERSION}" \`)
	line(b, `    --data-urlencode "started_at=${START_TIME}" \`)
	line(b, `    "${BORGMON_URL}/api/v1/runs" >/dev/null || true`)
	blank(b)
	return nil
}

// emitMonitorStats collects the metrics borgmon graphs, after the borg
// commands have run. Called from sectionBorg.
func emitMonitorStats(b *strings.Builder, job model.BackupJob) {
	if !monitoring(job) {
		return
	}
	line(b, `if [ "$MODE" = "real" ] && [ "$backup_exit" -lt 2 ]; then`)
	line(b, "    # Telemetry only, and additive: borg create is left exactly as it")
	line(b, "    # is, because --json would replace the text --stats block that")
	line(b, "    # on_exit scrapes for the notification.")
	line(b, "    #")
	line(b, "    # --glob-archives is mandatory here, not cosmetic: repositories are")
	line(b, "    # shared between jobs, and --last 1 unfiltered returns another")
	line(b, "    # host's archive — the graphs would then show someone else's data.")
	line(b, `    borg info --json --glob-archives "${BACKUPNAME}-*" --last 1 \`)
	line(b, `        > "$BORGMON_STATS" 2>/dev/null || true`)
	line(b, "fi")
	blank(b)
}

// emitMonitorFinish closes the run and ships the log. Called from on_exit, so
// it runs on every exit path including a signal.
func emitMonitorFinish(b *strings.Builder, job model.BackupJob) {
	if !monitoring(job) {
		return
	}
	line(b, "    # borgmon: close the run. Not gated on OnlyOnProblem or on the")
	line(b, "    # mode — this is a record, not a notification, and a monitor that")
	line(b, "    # only hears about failures cannot tell a healthy job from a")
	line(b, "    # silent one.")
	line(b, `    curl "${CURL_OPTS[@]}" -X POST \`)
	line(b, `        -H "Authorization: Bearer ${BORGMON_TOKEN}" \`)
	line(b, `        --data-urlencode "event=finish" \`)
	line(b, `        --data-urlencode "run_id=${RUN_ID}" \`)
	line(b, `        --data-urlencode "mode=${MODE}" \`)
	line(b, `        --data-urlencode "rc=${rc}" \`)
	line(b, `        --data-urlencode "backup_rc=${backup_exit}" \`)
	line(b, `        --data-urlencode "prune_rc=${prune_exit}" \`)
	line(b, `        --data-urlencode "compact_rc=${compact_exit}" \`)
	line(b, `        --data-urlencode "check_rc=${check_exit}" \`)
	line(b, `        --data-urlencode "duration_s=$(( $(date +%%s) - START_TIME ))" \`)
	line(b, `        --data-urlencode "skipped=${skipped}" \`)
	line(b, `        --data-urlencode "fail_reason=${FAIL_REASON}" \`)
	line(b, `        --data-urlencode "stats@${BORGMON_STATS}" \`)
	line(b, `        "${BORGMON_URL}/api/v1/runs" >/dev/null || true`)
	blank(b)
	line(b, "    # The log follows in its own request, with its own time box: a")
	line(b, "    # large upload must not be able to take the run record with it.")
	line(b, "    # LOG_OWNED gates it for the same reason every other read of the")
	line(b, "    # log does — a run that lost the lock race would otherwise ship")
	line(b, "    # the winning run's log as its own.")
	line(b, `    if [ "$LOG_OWNED" -eq 1 ] && [ -f "$LOG_FILE" ]; then`)
	line(b, `        curl --silent --show-error --connect-timeout 5 --max-time 120 \`)
	line(b, `            -X POST -H "Authorization: Bearer ${BORGMON_TOKEN}" \`)
	line(b, `            -H "X-Run-Id: ${RUN_ID}" \`)
	line(b, `            --data-binary "@${LOG_FILE}" \`)
	line(b, `            "${BORGMON_URL}/api/v1/runs/log" >/dev/null || true`)
	line(b, "    fi")
	line(b, `    rm -f "$BORGMON_STATS"`)
	blank(b)
}
