// Package model holds the parameter blocks of a single backup job.
//
// BackupJob is the only format the application persists: it is serialized into
// the "borggen:meta" block of every generated script, which makes generation,
// saving and re-import one mechanism.
package model

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Borg target versions. Options that exist only in 1.4 must never be emitted
// under target 1.2 — the generated script would die on argument parsing.
const (
	Borg12 = "1.2"
	Borg14 = "1.4"
)

// Transport kinds.
const (
	TransportLocal = "local"
	TransportSSHFS = "sshfs"
	TransportCIFS  = "cifs"
)

// Repo kinds.
const (
	RepoLocal = "local"
	RepoSSH   = "ssh"
)

// Secret modes.
const (
	SecretNone        = "none"
	SecretPassphrase  = "passphrase"
	SecretPasscommand = "passcommand"
)

// Log attachment modes.
const (
	AttachAlways  = "always"
	AttachOnError = "on_error"
	AttachNever   = "never"
)

// BackupJob is the complete, self-contained description of one backup job.
type BackupJob struct {
	Name       string `json:"name"`
	BorgTarget string `json:"borg_target"`

	Transport Transport `json:"transport"`
	Repo      Repo      `json:"repo"`
	Source    Source    `json:"source"`
	Retention Retention `json:"retention"`
	Verify    Verify    `json:"verify"`
	Notify    Notify    `json:"notify"`
	Runtime   Runtime   `json:"runtime"`

	// Monitor is a pointer, and nil when monitoring is off, so a job that
	// does not use borgmon carries no trace of it — not even
	// "enabled": false. Every other block is a value because its disabled
	// state still holds settings worth keeping; a disabled monitor holds
	// nothing at all.
	Monitor *Monitor `json:"monitor,omitempty"`
}

// Monitor points a generated script at a borgmon instance. The token is
// per-job, issued by borgmon, and lands in the script in clear text — the
// same accepted debt as the notification secrets, and the reason borgmon
// supports rotating it.
type Monitor struct {
	Enabled bool   `json:"enabled"`
	Token   string `json:"token,omitempty"`
}

// Transport describes how source data is made readable to borg.
type Transport struct {
	Kind string `json:"kind"`

	SSHFSRemote string   `json:"sshfs_remote,omitempty"`
	SSHFSMount  string   `json:"sshfs_mount,omitempty"`
	SSHKey      string   `json:"ssh_key,omitempty"`
	SSHFSOpts   []string `json:"sshfs_opts,omitempty"`

	CIFSShare string   `json:"cifs_share,omitempty"`
	CIFSMount string   `json:"cifs_mount,omitempty"`
	CIFSOpts  []string `json:"cifs_opts,omitempty"`

	// Unmount on exit. Forced true for sshfs (the mount exists for this job
	// only). Defaults to false for cifs: the share may serve other consumers,
	// so the script must not tear it down.
	UnmountOnExit bool `json:"unmount_on_exit"`
}

// Repo is the backup destination.
type Repo struct {
	Kind string `json:"kind"`
	Path string `json:"path"`

	SecretMode  string `json:"secret_mode"`
	SecretValue string `json:"secret_value,omitempty"`

	// BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK.
	AllowUnknownUnencrypted bool `json:"allow_unknown_unencrypted"`

	LockWait int `json:"lock_wait,omitempty"`
}

// Source is what gets archived.
type Source struct {
	// Paths are source-side: relative to the mount point for non-local
	// transports, absolute local paths for "local".
	Paths []string `json:"paths"`

	// Excludes are stored source-side too and are anchored to the mount point
	// at generation time.
	Excludes []string `json:"excludes,omitempty"`

	ExcludeCaches bool `json:"exclude_caches"`

	// ExcludeIfPresent generalizes ExcludeCaches: a directory containing a
	// filesystem object with any of these names is excluded, same mechanism
	// ExcludeCaches hardcodes for CACHEDIR.TAG. Entries are bare filenames,
	// not paths — checked at every directory level during the walk, not
	// anchored to a mount point the way Excludes patterns are.
	ExcludeIfPresent []string `json:"exclude_if_present,omitempty"`

	// KeepExcludeTags keeps the tag object itself in the archive when an
	// ExcludeIfPresent directory is otherwise dropped. Meaningless with an
	// empty ExcludeIfPresent; Normalize() clears it in that case.
	KeepExcludeTags bool `json:"keep_exclude_tags"`

	// OneFileSystem is a no-op over sshfs (SFTP carries no device numbers, so
	// the whole subtree reports one st_dev) and is refused for that transport.
	OneFileSystem bool `json:"one_file_system"`

	// ACLs/xattrs are metadata borg reads and stores by default. Some sources
	// (a CIFS mount, in particular) do not carry them reliably, in which case
	// reading them is wasted work rather than protection.
	NoACLs   bool `json:"no_acls"`
	NoXattrs bool `json:"no_xattrs"`

	Compression     string `json:"compression"`
	ArchiveTemplate string `json:"archive_template"`

	// CheckpointInterval writes a checkpoint archive every N seconds during a
	// long create, so a dropped connection over a slow link does not lose all
	// progress. Zero means borg's own default (1800s).
	CheckpointInterval int `json:"checkpoint_interval,omitempty"`
}

// Retention drives prune and compact.
type Retention struct {
	Prune bool `json:"prune"`

	KeepLast     int    `json:"keep_last,omitempty"`
	KeepMinutely int    `json:"keep_minutely,omitempty"`
	KeepHourly   int    `json:"keep_hourly,omitempty"`
	KeepDaily    int    `json:"keep_daily,omitempty"`
	KeepWeekly   int    `json:"keep_weekly,omitempty"`
	KeepMonthly  int    `json:"keep_monthly,omitempty"`
	KeepYearly   int    `json:"keep_yearly,omitempty"`
	KeepWithin   string `json:"keep_within,omitempty"`

	// 1.4-only.
	Keep13Weekly int `json:"keep_13weekly,omitempty"`
	Keep3Monthly int `json:"keep_3monthly,omitempty"`

	Compact          bool `json:"compact"`
	CompactThreshold int  `json:"compact_threshold,omitempty"`
}

// Verify is the borg check step. Only --repository-only is offered inside a
// backup script: a full check reads the whole repository and --verify-data
// decrypts every chunk, neither of which belongs in a daily job.
type Verify struct {
	Enabled     bool `json:"enabled"`
	MaxDuration int  `json:"max_duration,omitempty"`
}

// Notify holds the notification channels.
type Notify struct {
	Telegram TelegramNotify `json:"telegram"`
	Email    EmailNotify    `json:"email"`
	Webhook  WebhookNotify  `json:"webhook"`

	// Time boxes for every curl call. Not optional: a notification during host
	// shutdown can otherwise hang until systemd sends SIGKILL.
	CurlMaxTime        int `json:"curl_max_time"`
	CurlConnectTimeout int `json:"curl_connect_timeout"`
}

// TelegramNotify fans out to several chat IDs, as all working scripts do.
type TelegramNotify struct {
	Enabled bool     `json:"enabled"`
	Token   string   `json:"token,omitempty"`
	ChatIDs []string `json:"chat_ids,omitempty"`

	// AttachLogMode and OnlyOnProblem are per-channel: Telegram and Email
	// commonly want different rules (e.g. Telegram only on a problem, Email
	// always), not one setting shared by both.
	AttachLogMode string `json:"attach_log_mode"`
	// OnlyOnProblem skips a real run's success notification on this channel —
	// never applied to --check/--dry-run (those exist to prove the channel
	// works, spec §4.11) or to companion scripts (no run modes to gate on).
	OnlyOnProblem bool `json:"only_on_problem"`
}

// EmailNotify sends straight over SMTP with curl, so the generated script does
// not depend on a local MTA.
type EmailNotify struct {
	Enabled    bool   `json:"enabled"`
	To         string `json:"to,omitempty"`
	From       string `json:"from,omitempty"`
	SMTPServer string `json:"smtp_server,omitempty"`
	SMTPPort   int    `json:"smtp_port,omitempty"`
	SMTPUser   string `json:"smtp_user,omitempty"`
	SMTPPass   string `json:"smtp_pass,omitempty"`

	AttachLogMode string `json:"attach_log_mode"`
	OnlyOnProblem bool   `json:"only_on_problem"`
}

// WebhookNotify is the uptime-kuma push URL: a dead-man's switch covering what
// traps cannot (SIGKILL, OOM, power loss, host never booted).
//
// Never fired under --check or --dry-run: a test run must not feed the switch,
// or it masks the fact that the real backup failed.
type WebhookNotify struct {
	Enabled         bool   `json:"enabled"`
	PushURL         string `json:"push_url,omitempty"`
	NotifyOnFailure bool   `json:"notify_on_failure"`
}

// Runtime is execution plumbing. The run mode (--check / --dry-run / real) is
// an argument of the generated script, not a field here. LOG_FILE/LOCK_FILE
// are not fields either — always derived from the on-disk Filename at
// generation time (generator.Options.Filename), never user-overridable, so
// there is nothing here to round-trip or go stale on a rename.
type Runtime struct {
	// Reserved for the secrets migration; unused while secrets are inlined.
	SecretsFile string `json:"secrets_file,omitempty"`
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// compLevels are the level ranges documented in "borg help compression".
// Levels outside these ranges are rejected by borg itself.
var compLevels = map[string][2]int{
	"zstd": {1, 22},
	"zlib": {0, 9},
	"lzma": {0, 9},
}

// checkCompression validates a --compression specifier against the grammar in
// "borg help compression":
//
//	none | lz4 | (zstd|zlib|lzma)[,L] | auto,<inner> | obfuscate,SPEC,<inner>
//
// It returns an empty string when the specifier is valid.
func checkCompression(fields []string, allowAuto, allowObfuscate bool) string {
	if len(fields) == 0 || fields[0] == "" {
		return "must not be empty"
	}

	switch head := fields[0]; head {
	case "none", "lz4":
		if len(fields) > 1 {
			return head + " takes no level"
		}
		return ""

	case "zstd", "zlib", "lzma":
		if len(fields) > 2 {
			return "too many fields after " + head
		}
		if len(fields) == 2 {
			level, err := strconv.Atoi(fields[1])
			if err != nil {
				return fmt.Sprintf("%s level must be a number, got %q", head, fields[1])
			}
			r := compLevels[head]
			if level < r[0] || level > r[1] {
				return fmt.Sprintf("%s level must be %d..%d", head, r[0], r[1])
			}
		}
		return ""

	case "auto":
		if !allowAuto {
			return "auto cannot be nested"
		}
		return checkCompression(fields[1:], false, false)

	case "obfuscate":
		if !allowObfuscate {
			return "obfuscate cannot be nested"
		}
		if len(fields) < 3 {
			return "obfuscate needs SPEC and a compression specifier"
		}
		spec, err := strconv.Atoi(fields[1])
		if err != nil {
			return fmt.Sprintf("obfuscate SPEC must be a number, got %q", fields[1])
		}
		if !validObfuscateSpec(spec) {
			return "obfuscate SPEC must be 1..6 (relative), 110..123 (padding) or 250 (Padmé)"
		}
		return checkCompression(fields[2:], true, false)
	}

	return fmt.Sprintf("unknown algorithm %q", fields[0])
}

// validObfuscateSpec mirrors the three documented SPEC ranges.
func validObfuscateSpec(spec int) bool {
	return (spec >= 1 && spec <= 6) || (spec >= 110 && spec <= 123) || spec == 250
}

// UsesObfuscation reports whether the compression specifier pads chunk sizes.
func (j *BackupJob) UsesObfuscation() bool {
	return strings.HasPrefix(j.Source.Compression, "obfuscate,")
}

// patternStyles are the style selectors from "borg help patterns". A pattern
// carrying one of these is not a plain path and is left alone.
var patternStyles = []string{"fm:", "sh:", "re:", "pp:", "pf:"}

// LiteralPattern reports whether an exclude is a plain path: no style selector
// and no fnmatch metacharacter. Only these can be checked against the sources
// without reimplementing borg's matcher — "*.o" legitimately matches at any
// depth, so anything with a wildcard is left alone.
func LiteralPattern(p string) bool {
	for _, s := range patternStyles {
		if strings.HasPrefix(p, s) {
			return false
		}
	}
	return !strings.ContainsAny(p, "*?[")
}

// BorgPath normalizes a path the way borg does before matching: "a leading
// path separator is always removed", and archived paths never start with one
// either — /home/user is archived as home/user.
func BorgPath(p string) string { return strings.Trim(p, "/") }

// ExcludeReaches reports whether a literal pattern can match anything under the
// given sources. A pattern above a source still matches (it excludes the whole
// subtree); one that is simply elsewhere in the filesystem can never match.
func ExcludeReaches(pattern string, sources []string) bool {
	pat := BorgPath(pattern)
	if pat == "" {
		return true
	}
	for _, src := range sources {
		s := BorgPath(src)
		if s == "" || pat == s ||
			strings.HasPrefix(pat, s+"/") || strings.HasPrefix(s, pat+"/") {
			return true
		}
	}
	return false
}

// DefaultJob returns a job pre-filled with the conventions the working scripts
// already follow, so a fresh form generates something runnable.
func DefaultJob() BackupJob {
	return BackupJob{
		Name:       "backup",
		BorgTarget: Borg12,
		Transport:  Transport{Kind: TransportLocal},
		Repo: Repo{
			Kind:                    RepoLocal,
			Path:                    "/backup/secure/borg-backup",
			SecretMode:              SecretNone,
			AllowUnknownUnencrypted: true,
		},
		Source: Source{
			Paths:           []string{"/etc"},
			ExcludeCaches:   true,
			Compression:     "lz4",
			ArchiveTemplate: "{name}-{now:%Y-%m-%d_%H:%M}",
		},
		Retention: Retention{
			Prune:       true,
			KeepDaily:   7,
			KeepWeekly:  4,
			KeepMonthly: 6,
			Compact:     true,
		},
		Verify: Verify{Enabled: false, MaxDuration: 3600},
		Notify: Notify{
			CurlMaxTime:        10,
			CurlConnectTimeout: 5,
		},
		Runtime: Runtime{},
	}
}

// Normalize fills in derived defaults and enforces the invariants that are not
// user choices. It is called before validation and before generation, so the
// same job always produces the same script.
func (j *BackupJob) Normalize() {
	j.Name = strings.TrimSpace(j.Name)
	if j.BorgTarget == "" {
		j.BorgTarget = Borg12
	}
	if j.Transport.Kind == "" {
		j.Transport.Kind = TransportLocal
	}
	if j.Repo.Kind == "" {
		j.Repo.Kind = RepoLocal
	}
	if j.Repo.SecretMode == "" {
		j.Repo.SecretMode = SecretNone
	}
	if j.Source.Compression == "" {
		j.Source.Compression = "lz4"
	}
	if j.Source.ArchiveTemplate == "" {
		j.Source.ArchiveTemplate = "{name}-{now:%Y-%m-%d_%H:%M}"
	}
	if j.Notify.Telegram.AttachLogMode == "" {
		j.Notify.Telegram.AttachLogMode = AttachOnError
	}
	if j.Notify.Email.AttachLogMode == "" {
		j.Notify.Email.AttachLogMode = AttachOnError
	}
	if j.Notify.CurlMaxTime <= 0 {
		j.Notify.CurlMaxTime = 10
	}
	if j.Notify.CurlConnectTimeout <= 0 {
		j.Notify.CurlConnectTimeout = 5
	}
	if j.Verify.Enabled && j.Verify.MaxDuration <= 0 {
		j.Verify.MaxDuration = 3600
	}

	switch j.Transport.Kind {
	case TransportSSHFS:
		// The mount exists for this job only, so it is always torn down.
		j.Transport.UnmountOnExit = true
		if j.Transport.SSHFSMount == "" && j.Name != "" {
			j.Transport.SSHFSMount = "/tmp/sshfs-" + j.Name
		}
		if len(j.Transport.SSHFSOpts) == 0 {
			j.Transport.SSHFSOpts = []string{"ro", "reconnect", "ServerAliveInterval=15", "ServerAliveCountMax=3"}
		}
		// No-op over sshfs; refuse it rather than imply protection.
		j.Source.OneFileSystem = false
		j.clearCIFS()
	case TransportCIFS:
		if len(j.Transport.CIFSOpts) == 0 {
			j.Transport.CIFSOpts = []string{"ro", "sec=ntlmv2", "vers=3.0"}
		}
		j.clearSSHFS()
	default:
		j.Transport.Kind = TransportLocal
		j.Transport.UnmountOnExit = false
		j.clearSSHFS()
		j.clearCIFS()
	}

	if j.BorgTarget == Borg12 {
		// Gate 1.4-only retention options out of the model entirely, so they
		// cannot leak into the script or the meta block.
		j.Retention.Keep13Weekly = 0
		j.Retention.Keep3Monthly = 0
	}
	if !j.Retention.Prune {
		j.Retention.KeepDaily, j.Retention.KeepWeekly = 0, 0
		j.Retention.KeepMonthly, j.Retention.KeepYearly = 0, 0
		j.Retention.KeepLast, j.Retention.KeepMinutely, j.Retention.KeepHourly = 0, 0, 0
		j.Retention.KeepWithin = ""
		j.Retention.Keep13Weekly, j.Retention.Keep3Monthly = 0, 0
	}
	if j.Repo.SecretMode == SecretNone {
		j.Repo.SecretValue = ""
	}

	j.Source.Paths = trimAll(j.Source.Paths)
	j.Source.Excludes = trimAll(j.Source.Excludes)
	j.Source.ExcludeIfPresent = trimAll(j.Source.ExcludeIfPresent)
	if len(j.Source.ExcludeIfPresent) == 0 {
		j.Source.KeepExcludeTags = false
	}
	j.Notify.Telegram.ChatIDs = trimAll(j.Notify.Telegram.ChatIDs)

	// A disabled channel keeps none of its configuration. Without this, a
	// script imported as a template for a new job carried the old job's
	// Telegram token, chat IDs and push URL into the meta block of a script
	// that never uses them — invisible in the form, and committed to the
	// library's git history. Same rule the repo secret already follows above;
	// this only extends it to the place it was missing.
	//
	// The shared curl timeouts are deliberately not cleared: they belong to
	// Notify rather than to any one channel, and have non-zero defaults that
	// would be silently restored on the next import.
	if !j.Notify.Telegram.Enabled {
		j.Notify.Telegram.Token = ""
		j.Notify.Telegram.ChatIDs = nil
	}
	if !j.Notify.Email.Enabled {
		j.Notify.Email.To, j.Notify.Email.From = "", ""
		j.Notify.Email.SMTPServer, j.Notify.Email.SMTPPort = "", 0
		j.Notify.Email.SMTPUser, j.Notify.Email.SMTPPass = "", ""
	}
	if !j.Notify.Webhook.Enabled {
		// The push URL is itself a credential: the token sits in its path.
		j.Notify.Webhook.PushURL = ""
	}

	if j.Monitor != nil && !j.Monitor.Enabled {
		j.Monitor = nil
	}
}

func (j *BackupJob) clearSSHFS() {
	j.Transport.SSHFSRemote, j.Transport.SSHFSMount, j.Transport.SSHKey = "", "", ""
	j.Transport.SSHFSOpts = nil
}

func (j *BackupJob) clearCIFS() {
	j.Transport.CIFSShare, j.Transport.CIFSMount = "", ""
	j.Transport.CIFSOpts = nil
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Problem is one validation failure. Field carries the model path that caused
// it, so the UI can put the message next to the offending control instead of
// making the user map a message back to a form field by hand.
type Problem struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (p Problem) String() string { return p.Field + ": " + p.Message }

// excludeHint explains why a pattern is inert and offers the two ways to fix
// it: the full path, or the wildcard form borg's own docs recommend.
func excludeHint(pattern string, sources []string) string {
	msg := fmt.Sprintf("%q cannot match anything under %s",
		pattern, strings.Join(sources, ", "))
	if len(sources) == 1 {
		msg += fmt.Sprintf(" — use %q",
			"/"+BorgPath(sources[0])+"/"+BorgPath(pattern))
	}
	return msg + fmt.Sprintf(" or a wildcard like %q", "*/"+path.Base(BorgPath(pattern)))
}

// Validate reports every problem that would produce a broken script. It never
// stops at the first one: the UI shows them all at once.
func (j *BackupJob) Validate() []Problem {
	var out []Problem
	add := func(field, message string) {
		out = append(out, Problem{Field: field, Message: message})
	}

	if j.Name == "" {
		add("name", "must not be empty")
	} else if !nameRe.MatchString(j.Name) {
		add("name", "only letters, digits, dot, dash and underscore are allowed")
	}
	if j.BorgTarget != Borg12 && j.BorgTarget != Borg14 {
		add("borg_target", "only 1.2 and 1.4 are supported")
	}
	if j.Repo.Path == "" {
		add("repo.path", "must not be empty")
	}
	if j.Repo.Kind == RepoSSH && !strings.HasPrefix(j.Repo.Path, "ssh://") {
		add("repo.path", "an ssh repository path must start with ssh://")
	}
	if j.Repo.Kind == RepoLocal && strings.HasPrefix(j.Repo.Path, "ssh://") {
		add("repo.kind", "an ssh:// path requires kind=ssh")
	}
	if j.Repo.SecretMode != SecretNone && j.Repo.SecretValue == "" {
		add("repo.secret_value", "empty value with secret_mode="+j.Repo.SecretMode)
	}
	if len(j.Source.Paths) == 0 {
		add("source.paths", "at least one path is required")
	}
	// An exclude that cannot match anything fails silently: the backup runs,
	// reports success, and quietly contains what you meant to leave out.
	for _, ex := range j.Source.Excludes {
		if !LiteralPattern(ex) || ExcludeReaches(ex, j.Source.Paths) {
			continue
		}
		add("source.excludes", excludeHint(ex, j.Source.Paths))
	}
	// --exclude-if-present NAME is a bare tag filename, not a path — borg
	// checks every directory in the walk for an object with this exact name,
	// nothing to anchor to a mount point. A "/" here would misleadingly
	// suggest a specific location can be given.
	for _, tag := range j.Source.ExcludeIfPresent {
		if strings.Contains(tag, "/") {
			add("source.exclude_if_present", "must be a plain filename, not a path: "+tag)
		}
	}
	if msg := checkCompression(strings.Split(j.Source.Compression, ","), true, true); msg != "" {
		add("source.compression", msg)
	}
	// Documented as a hard requirement: obfuscation hides the compressed size,
	// which tells an observer nothing unless the data itself is encrypted. On a
	// plaintext repo it only inflates the repository.
	if j.UsesObfuscation() && j.Repo.SecretMode == SecretNone {
		add("source.compression", "obfuscate is only meaningful on an encrypted repository — set a passphrase or drop it")
	}
	if !strings.Contains(j.Source.ArchiveTemplate, "{now") && !strings.Contains(j.Source.ArchiveTemplate, "{utcnow") {
		add("source.archive_template", "without {now:...} every archive gets the same name")
	}
	// prune matches "<name>-*"; a template that does not start with the job
	// name would leave archives unpruned or, worse, match somebody else's.
	if !strings.HasPrefix(j.Source.ArchiveTemplate, "{name}-") {
		add("source.archive_template", "must start with {name}- because prune filters archives by that prefix")
	}

	switch j.Transport.Kind {
	case TransportSSHFS:
		if j.Transport.SSHFSRemote == "" {
			add("transport.sshfs_remote", "must not be empty")
		} else if !strings.Contains(j.Transport.SSHFSRemote, ":") {
			add("transport.sshfs_remote", "expected the form user@host:/path")
		}
		if j.Transport.SSHFSMount == "" {
			add("transport.sshfs_mount", "must not be empty")
		}
	case TransportCIFS:
		if j.Transport.CIFSShare == "" {
			add("transport.cifs_share", "must not be empty")
		} else if !strings.HasPrefix(j.Transport.CIFSShare, "//") {
			add("transport.cifs_share", "expected the form //host/share")
		}
		if j.Transport.CIFSMount == "" {
			add("transport.cifs_mount", "must not be empty")
		}
	}

	if j.Retention.Prune && j.Retention.KeepDaily == 0 && j.Retention.KeepWeekly == 0 &&
		j.Retention.KeepMonthly == 0 && j.Retention.KeepYearly == 0 && j.Retention.KeepWithin == "" &&
		j.Retention.KeepLast == 0 && j.Retention.KeepMinutely == 0 && j.Retention.KeepHourly == 0 &&
		j.Retention.Keep13Weekly == 0 && j.Retention.Keep3Monthly == 0 {
		add("retention.prune", "prune is enabled but no keep rule is set — this would delete every archive of the job")
	}
	if j.Monitor != nil && j.Monitor.Enabled && j.Monitor.Token == "" {
		add("monitor.token", "monitoring is enabled but no token is set — borgmon rejects the push and the run is never recorded")
	}
	if j.BorgTarget == Borg12 && (j.Retention.Keep13Weekly > 0 || j.Retention.Keep3Monthly > 0) {
		add("retention.keep_13weekly", "--keep-13weekly/--keep-3monthly exist only in borg 1.4")
	}

	if j.Notify.Telegram.Enabled {
		if j.Notify.Telegram.Token == "" {
			add("notify.telegram.token", "must not be empty")
		}
		if len(j.Notify.Telegram.ChatIDs) == 0 {
			add("notify.telegram.chat_ids", "at least one ID is required")
		}
	}
	if j.Notify.Email.Enabled {
		if j.Notify.Email.To == "" {
			add("notify.email.to", "must not be empty")
		}
		if j.Notify.Email.From == "" {
			add("notify.email.from", "must not be empty")
		}
		if j.Notify.Email.SMTPServer == "" {
			add("notify.email.smtp_server", "must not be empty")
		}
		if j.Notify.Email.SMTPPort <= 0 {
			add("notify.email.smtp_port", "must be positive")
		}
	}
	if j.Notify.Webhook.Enabled && j.Notify.Webhook.PushURL == "" {
		add("notify.webhook.push_url", "must not be empty")
	}
	switch j.Notify.Telegram.AttachLogMode {
	case AttachAlways, AttachOnError, AttachNever:
	default:
		add("notify.telegram.attach_log_mode", fmt.Sprintf("unknown mode %q", j.Notify.Telegram.AttachLogMode))
	}
	switch j.Notify.Email.AttachLogMode {
	case AttachAlways, AttachOnError, AttachNever:
	default:
		add("notify.email.attach_log_mode", fmt.Sprintf("unknown mode %q", j.Notify.Email.AttachLogMode))
	}

	return out
}

// ArchiveName resolves the {name} placeholder of the archive template. Borg's
// own placeholders ({now}, {hostname}, ...) are left untouched.
func (j *BackupJob) ArchiveName() string {
	return strings.ReplaceAll(j.Source.ArchiveTemplate, "{name}", j.Name)
}

// MountPoint returns the directory sources are read from, empty for local.
func (j *BackupJob) MountPoint() string {
	switch j.Transport.Kind {
	case TransportSSHFS:
		return j.Transport.SSHFSMount
	case TransportCIFS:
		return j.Transport.CIFSMount
	}
	return ""
}
