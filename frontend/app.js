'use strict';

// Marker strings must stay identical to internal/generator/generator.go.
const USER_PRE_BEGIN = '# --- borggen:user:pre BEGIN ---';
const USER_PRE_END = '# --- borggen:user:pre END ---';
const USER_POST_BEGIN = '# --- borggen:user:post BEGIN ---';
const USER_POST_END = '# --- borggen:user:post END ---';

// The target borg version is fixed: 1.2 is what the production hosts run, and a
// 1.2 script also runs on 1.4. The field stays in the model and in the meta
// block so the generated script records what it was built for.
const BORG_TARGET = '1.2';

// Level ranges from "borg help compression". none and lz4 take no level.
const COMP_LEVELS = {
  zstd: { min: 1, max: 22, def: 3 },
  zlib: { min: 0, max: 9, def: 6 },
  lzma: { min: 0, max: 9, def: 6 },
};

const $ = (id) => document.getElementById(id);
const lines = (s) => s.split('\n').map((v) => v.trim()).filter((v) => v !== '');
const num = (id) => parseInt($(id).value, 10) || 0;

// compressionSpec assembles the --compression argument from the controls.
// Grammar: [obfuscate,SPEC,][auto,]ALGO[,LEVEL]
function compressionSpec() {
  const algo = $('compression_algo').value;
  const level = $('compression_level').value.trim();

  let spec = algo;
  if (COMP_LEVELS[algo] && level !== '') spec = `${algo},${level}`;
  if ($('compression_auto').checked) spec = `auto,${spec}`;
  if ($('compression_obfuscate').checked) {
    const s = $('compression_spec').value.trim();
    spec = `obfuscate,${s === '' ? '1' : s},${spec}`;
  }
  return spec;
}

// applyCompression decomposes a stored specifier back into the controls, so an
// imported script or a restored draft round-trips through the form.
function applyCompression(spec) {
  let f = (spec || 'lz4').split(',');

  const obfuscate = f[0] === 'obfuscate';
  $('compression_obfuscate').checked = obfuscate;
  $('compression_spec').value = obfuscate ? (f[1] || '') : '';
  if (obfuscate) f = f.slice(2);

  const auto = f[0] === 'auto';
  $('compression_auto').checked = auto;
  if (auto) f = f.slice(1);

  $('compression_algo').value = COMP_LEVELS[f[0]] || f[0] === 'none' || f[0] === 'lz4' ? f[0] : 'lz4';
  $('compression_level').value = f[1] || '';
}

let editorDirty = false;
let formChangedWhileDirty = false;
let suppressDraft = true;

// Filename follows BACKUPNAME (like a slug following a title) only until the
// user edits it directly, or a real name arrives from import/git-load/a
// restored draft — see filenameBase() and the BACKUPNAME input listener.
let filenameTouched = false;

// Ed wraps the Monaco instance so the rest of the app does not care whether
// the editor is a textarea or Monaco. programmaticSet guards against the
// change event firing when *we* replace the content (regeneration, import).
const Ed = {
  monaco: null,
  programmatic: false,
  onChange: null,
  get() { return this.monaco ? this.monaco.getValue() : ''; },
  set(text) {
    if (!this.monaco) return;
    const next = text || '';
    // Regeneration fires on every field change. Replacing identical content
    // would still scroll the view, so skip the no-op entirely.
    if (this.monaco.getValue() === next) return;

    // Keep the reader where they were: without this, editing any form field
    // throws you back to line 1 of the script.
    const view = this.monaco.saveViewState();
    this.programmatic = true;
    this.monaco.setValue(next);
    this.programmatic = false;
    if (view) this.monaco.restoreViewState(view);
  },
};

// initEditor loads Monaco from the offline vendor bundle. The worker is served
// same-origin through a tiny blob proxy, so no CDN is ever contacted.
function initEditor() {
  return new Promise((resolve) => {
    self.MonacoEnvironment = {
      getWorkerUrl() {
        const base = new URL('vendor/monaco/vs/', location.href).href;
        const code =
          'self.MonacoEnvironment={baseUrl:"' + base + '"};' +
          'importScripts("' + base + 'base/worker/workerMain.js");';
        return URL.createObjectURL(new Blob([code], { type: 'text/javascript' }));
      },
    };
    require.config({ paths: { vs: 'vendor/monaco/vs' } });
    require(['vs/editor/editor.main'], () => {
      // vs-dark's own scrollbar slider is a semi-transparent light gray —
      // reads as an off-theme pale bar against this palette. Extending the
      // theme just to retint the slider (and match the editor background to
      // --inset, the same shade every other code-like surface uses) keeps
      // everything else about vs-dark's syntax highlighting untouched.
      monaco.editor.defineTheme('borggen-dark', {
        base: 'vs-dark',
        inherit: true,
        rules: [],
        colors: {
          'editor.background': '#0f1115',
          'scrollbarSlider.background': '#2c313a99',
          'scrollbarSlider.hoverBackground': '#3a4150cc',
          'scrollbarSlider.activeBackground': '#6ea8fe99',
        },
      });
      Ed.monaco = monaco.editor.create($('script'), {
        value: '',
        language: 'shell',
        theme: 'borggen-dark',
        readOnly: false,
        automaticLayout: true,
        minimap: { enabled: false },
        fontSize: 12.5,
        scrollBeyondLastLine: false,
        renderWhitespace: 'none',
        tabSize: 4,
      });
      Ed.monaco.onDidChangeModelContent(() => {
        if (Ed.programmatic) return;
        if (Ed.onChange) Ed.onChange();
      });
      resolve();
    });
  });
}

function collect() {
  return {
    name: $('name').value.trim(),
    borg_target: BORG_TARGET,
    transport: {
      kind: $('transport_kind').value,
      sshfs_remote: $('sshfs_remote').value.trim(),
      sshfs_mount: $('sshfs_mount').value.trim(),
      ssh_key: $('ssh_key').value.trim(),
      sshfs_opts: lines($('sshfs_opts').value.replace(/,/g, '\n')),
      cifs_share: $('cifs_share').value.trim(),
      cifs_mount: $('cifs_mount').value.trim(),
      cifs_opts: lines($('cifs_opts').value.replace(/,/g, '\n')),
      unmount_on_exit: $('unmount_on_exit').checked,
    },
    repo: {
      kind: $('repo_kind').value,
      path: $('repo_path').value.trim(),
      secret_mode: $('secret_mode').value,
      secret_value: $('secret_value').value,
      allow_unknown_unencrypted: $('allow_unknown').checked,
      lock_wait: $('lock_wait_enabled').checked ? num('lock_wait') : 0,
    },
    source: {
      paths: lines($('paths').value),
      excludes: lines($('excludes').value),
      exclude_caches: $('exclude_caches').checked,
      exclude_if_present: $('exclude_if_present_enabled').checked ? lines($('exclude_if_present').value) : [],
      keep_exclude_tags: $('exclude_if_present_enabled').checked && $('keep_exclude_tags').checked,
      one_file_system: $('one_file_system').checked,
      no_acls: $('no_acls').checked,
      no_xattrs: $('no_xattrs').checked,
      compression: compressionSpec(),
      archive_template: $('archive_template').value.trim(),
      checkpoint_interval: num('checkpoint_interval'),
    },
    retention: {
      prune: $('prune').checked,
      keep_last: num('keep_last'),
      keep_minutely: num('keep_minutely'),
      keep_hourly: num('keep_hourly'),
      keep_daily: num('keep_daily'),
      keep_weekly: num('keep_weekly'),
      keep_monthly: num('keep_monthly'),
      keep_yearly: num('keep_yearly'),
      keep_within: $('keep_within').value.trim(),
      compact: $('compact').checked,
      compact_threshold: num('compact_threshold'),
    },
    verify: {
      enabled: $('verify_enabled').checked,
      max_duration: num('verify_max_duration'),
    },
    notify: {
      telegram: {
        enabled: $('tg_enabled').checked,
        token: $('tg_token').value.trim(),
        chat_ids: lines($('tg_chats').value),
        attach_log_mode: $('tg_attach_log_mode').value,
        only_on_problem: $('tg_only_on_problem').checked,
      },
      email: {
        enabled: $('mail_enabled').checked,
        to: $('mail_to').value.trim(),
        from: $('mail_from').value.trim(),
        smtp_server: $('smtp_server').value.trim(),
        smtp_port: num('smtp_port'),
        smtp_user: $('smtp_user').value.trim(),
        smtp_pass: $('smtp_pass').value,
        attach_log_mode: $('mail_attach_log_mode').value,
        only_on_problem: $('mail_only_on_problem').checked,
      },
      webhook: {
        enabled: $('wh_enabled').checked,
        push_url: $('wh_url').value.trim(),
        notify_on_failure: $('wh_on_failure').checked,
      },
      curl_max_time: num('curl_max'),
      curl_connect_timeout: num('curl_connect'),
    },
    // Absent, not {enabled:false}: the server drops a disabled monitor
    // entirely, so a job that does not use borgmon carries no trace of it.
    monitor: $('monitor_enabled').checked
      ? { enabled: true, token: $('monitor_token').value }
      : null,
  };
}

function apply(job) {
  const t = job.transport || {};
  const r = job.repo || {};
  const s = job.source || {};
  const ret = job.retention || {};
  const v = job.verify || {};
  const n = job.notify || {};
  const tg = n.telegram || {};
  const mail = n.email || {};
  const wh = n.webhook || {};

  $('name').value = job.name || '';

  $('transport_kind').value = t.kind || 'local';
  $('sshfs_remote').value = t.sshfs_remote || '';
  $('sshfs_mount').value = t.sshfs_mount || '';
  $('ssh_key').value = t.ssh_key || '';
  $('sshfs_opts').value = (t.sshfs_opts || []).join(',');
  $('cifs_share').value = t.cifs_share || '';
  $('cifs_mount').value = t.cifs_mount || '';
  $('cifs_opts').value = (t.cifs_opts || []).join(',');
  $('unmount_on_exit').checked = !!t.unmount_on_exit;

  $('repo_kind').value = r.kind || 'local';
  $('repo_path').value = r.path || '';
  $('secret_mode').value = r.secret_mode || 'none';
  $('secret_value').value = r.secret_value || '';
  $('allow_unknown').checked = !!r.allow_unknown_unencrypted;
  $('lock_wait_enabled').checked = (r.lock_wait || 0) > 0;
  $('lock_wait').value = r.lock_wait || 0;

  $('paths').value = (s.paths || []).join('\n');
  $('excludes').value = (s.excludes || []).join('\n');
  $('exclude_caches').checked = !!s.exclude_caches;
  $('monitor_enabled').checked = !!(job.monitor && job.monitor.enabled);
  $('monitor_token').value = (job.monitor && job.monitor.token) || '';
  $('exclude_if_present_enabled').checked = (s.exclude_if_present || []).length > 0;
  $('exclude_if_present').value = (s.exclude_if_present || []).join('\n');
  $('keep_exclude_tags').checked = !!s.keep_exclude_tags;
  $('one_file_system').checked = !!s.one_file_system;
  $('no_acls').checked = !!s.no_acls;
  $('no_xattrs').checked = !!s.no_xattrs;
  applyCompression(s.compression);
  $('archive_template').value = s.archive_template || '';
  $('checkpoint_interval').value = s.checkpoint_interval || 0;

  $('prune').checked = !!ret.prune;
  $('keep_last').value = ret.keep_last || 0;
  $('keep_minutely').value = ret.keep_minutely || 0;
  $('keep_hourly').value = ret.keep_hourly || 0;
  $('keep_daily').value = ret.keep_daily || 0;
  $('keep_weekly').value = ret.keep_weekly || 0;
  $('keep_monthly').value = ret.keep_monthly || 0;
  $('keep_yearly').value = ret.keep_yearly || 0;
  $('keep_within').value = ret.keep_within || '';
  $('compact').checked = !!ret.compact;
  $('compact_threshold').value = ret.compact_threshold || 0;

  $('verify_enabled').checked = !!v.enabled;
  $('verify_max_duration').value = v.max_duration || 3600;

  $('tg_enabled').checked = !!tg.enabled;
  $('tg_token').value = tg.token || '';
  $('tg_chats').value = (tg.chat_ids || []).join('\n');
  $('tg_attach_log_mode').value = tg.attach_log_mode || 'on_error';
  $('tg_only_on_problem').checked = !!tg.only_on_problem;
  $('mail_enabled').checked = !!mail.enabled;
  $('mail_to').value = mail.to || '';
  $('mail_from').value = mail.from || '';
  $('smtp_server').value = mail.smtp_server || '';
  $('smtp_port').value = mail.smtp_port || 465;
  $('smtp_user').value = mail.smtp_user || '';
  $('smtp_pass').value = mail.smtp_pass || '';
  $('mail_attach_log_mode').value = mail.attach_log_mode || 'on_error';
  $('mail_only_on_problem').checked = !!mail.only_on_problem;
  $('wh_enabled').checked = !!wh.enabled;
  $('wh_url').value = wh.push_url || '';
  $('wh_on_failure').checked = !!wh.notify_on_failure;
  $('curl_max').value = n.curl_max_time || 10;
  $('curl_connect').value = n.curl_connect_timeout || 5;

  syncVisibility();
  renderPreviews();
}

// syncVisibility hides controls that do not apply, rather than letting the
// user set something that silently does nothing.
function syncVisibility() {
  const kind = $('transport_kind').value;
  $('tr_sshfs').style.display = kind === 'sshfs' ? '' : 'none';
  $('tr_cifs').style.display = kind === 'cifs' ? '' : 'none';

  // --one-file-system relies on st_dev changing at a mount boundary; over
  // sshfs the whole subtree reports one device, so the option is a no-op.
  $('ofs_row').style.display = kind === 'sshfs' ? 'none' : '';

  $('exclude_if_present_box').style.display = $('exclude_if_present_enabled').checked ? '' : 'none';

  $('secret_value_row').style.display = $('secret_mode').value === 'none' ? 'none' : '';

  // BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK only answers borg's "Attempting
  // to access a previously unknown unencrypted repository" prompt (borg docs,
  // "Miscellaneous Help" / automatic answerers) — which can only fire against
  // a repo that genuinely has no key. Selecting a passphrase/passcommand here
  // asserts the repo is encrypted, so that prompt never triggers and the
  // checkbox has no observable effect.
  $('allow_unknown_row').style.display = $('secret_mode').value === 'none' ? '' : 'none';
  $('lock_wait_row').style.display = $('lock_wait_enabled').checked ? '' : 'none';
  $('tg_box').style.display = $('tg_enabled').checked ? '' : 'none';
  $('mail_box').style.display = $('mail_enabled').checked ? '' : 'none';
  $('wh_box').style.display = $('wh_enabled').checked ? '' : 'none';

  // Retention/Verify fields that Normalize() zeroes out server-side whenever
  // their gating checkbox is off — hidden here so the form does not invite
  // filling in numbers that generate() is about to discard.
  $('keep_fields').style.display = $('prune').checked ? '' : 'none';
  $('compact_threshold_row').style.display = $('compact').checked ? '' : 'none';
  $('verify_max_duration_row').style.display = $('verify_enabled').checked ? '' : 'none';

  // CURL_OPTS is shared by all three channels (generator.go), and notifyFuncs()
  // emits nothing at all — not even the array's one use site — once every
  // channel is disabled. Timeouts are dead configuration in that state.
  $('monitor_box').style.display = $('monitor_enabled').checked ? '' : 'none';

  // CURL_OPTS is shared by the notify channels and by borgmon, and the
  // generator emits it only when one of them will use it.
  $('curl_opts_row').style.display =
    ($('tg_enabled').checked || $('mail_enabled').checked || $('wh_enabled').checked
     || $('monitor_enabled').checked) ? '' : 'none';

  // A level only exists for the algorithms that take one.
  const algo = $('compression_algo').value;
  const range = COMP_LEVELS[algo];
  $('comp_level_row').style.display = range ? '' : 'none';
  if (range) {
    const level = $('compression_level');
    level.min = range.min;
    level.max = range.max;
    level.placeholder = `default ${range.def}`;
    $('comp_level_range').textContent = `${range.min}..${range.max}`;
  }
  $('comp_spec_row').style.display = $('compression_obfuscate').checked ? '' : 'none';

  // auto,C falls back to "none" for incompressible chunks; with algo itself
  // set to "none", every chunk already takes that fallback, so "auto,none"
  // behaves identically to "none" in every case (borg docs, "borg help
  // compression"). The checkbox has nothing to switch between.
  $('comp_auto_row').style.display = algo === 'none' ? 'none' : '';

  const mountName = kind === 'sshfs' ? '${SSHFS_MOUNT}' : kind === 'cifs' ? '${CIFS_MOUNT}' : '';
  $('anchor_hint').textContent = mountName
    ? `Enter paths as they look on the source — the generator prepends ${mountName}.`
    : 'Local absolute paths.';
}

function extractZone(text, begin, end) {
  const i = text.indexOf(begin);
  if (i < 0) return '';
  const from = i + begin.length;
  const j = text.indexOf(end, from);
  if (j < 0) return '';
  return text.slice(from, j).replace(/^\n+|\n+$/g, '');
}

// The Before/After Runtime fields are the source generate() sends — not the
// editor — so typing there behaves like any other form field (change, then
// Regenerate). This re-derives them from a script's actual zone content
// (import, restored draft, freshly generated output), so they never go
// stale relative to whatever the editor is really holding.
function syncZoneFields(text) {
  $('runtime_pre').value = extractZone(text, USER_PRE_BEGIN, USER_PRE_END);
  $('runtime_post').value = extractZone(text, USER_POST_BEGIN, USER_POST_END);
}

function setStatus(msg, cls) {
  const el = $('status');
  el.textContent = msg;
  el.className = 'status' + (cls ? ' ' + cls : '');
}

// flashDone briefly swaps a toolbar button's leading icon for a checkmark —
// confirmation that a click actually did something, right on the button
// itself, without waiting on the (easy to miss) status line. Every label
// here is exactly "<icon> <rest>", so replacing just the first character is
// enough. clearTimeout guards a rapid second click from restoring the label
// too early, mid-flash.
function flashDone(btn) {
  if (btn._flashTimer) clearTimeout(btn._flashTimer);
  if (!btn._flashOriginal) btn._flashOriginal = btn.textContent;
  btn.textContent = '✓' + btn._flashOriginal.slice(1);
  btn._flashTimer = setTimeout(() => {
    btn.textContent = btn._flashOriginal;
    btn._flashTimer = null;
  }, 1000);
}

// showProblems puts each message next to the control that caused it, using the
// data-field path the backend reports. Anything that has no matching control
// falls back to the summary list above the editor.
function showProblems(list) {
  document.querySelectorAll('.field-error').forEach((el) => el.remove());
  document.querySelectorAll('.invalid').forEach((el) => el.classList.remove('invalid'));

  const ul = $('problems');
  ul.innerHTML = '';

  (list || []).forEach((p) => {
    const field = p && p.field ? p.field : '';
    const message = p && p.message ? p.message : String(p);
    const control = field ? document.querySelector(`[data-field="${field}"]`) : null;

    if (control) {
      control.classList.add('invalid');
      const note = document.createElement('div');
      note.className = 'field-error';
      note.textContent = message;
      (control.closest('label') || control).insertAdjacentElement('afterend', note);
      return;
    }
    const li = document.createElement('li');
    li.textContent = field ? `${field}: ${message}` : message;
    ul.appendChild(li);
  });
}

// anchorPreview mirrors anchor() in internal/generator/generator.go. The two
// must stay in sync: this is the line the user is shown, and a preview that
// lies is worse than no preview.
function anchorPreview(pattern) {
  const kind = $('transport_kind').value;
  const v = kind === 'sshfs' ? 'SSHFS_MOUNT' : kind === 'cifs' ? 'CIFS_MOUNT' : '';
  const esc = (s) => s.replace(/\\/g, '\\\\').replace(/"/g, '\\"').replace(/`/g, '\\`').replace(/\$/g, '\\$');

  if (!v) return `"${esc(pattern)}"`;
  const prefix = '${' + v + '}';
  if (pattern === '/' || pattern === '') return `"${prefix}"`;
  if (pattern.startsWith('/')) return `"${prefix}${esc(pattern)}"`;
  return `"${prefix}/${esc(pattern)}"`;
}

// Mirrors LiteralPattern/BorgPath/ExcludeReaches in internal/model/params.go.
// A pattern that cannot match anything fails silently at backup time, so the
// form has to say so before the script is ever written.
const PATTERN_STYLES = ['fm:', 'sh:', 're:', 'pp:', 'pf:'];

const borgPath = (p) => p.replace(/^\/+/, '').replace(/\/+$/, '');

function literalPattern(p) {
  if (PATTERN_STYLES.some((s) => p.startsWith(s))) return false;
  return !/[*?[]/.test(p);
}

function excludeReaches(pattern, sources) {
  const pat = borgPath(pattern);
  if (pat === '') return true;
  return sources.some((src) => {
    const s = borgPath(src);
    return s === '' || pat === s || pat.startsWith(s + '/') || s.startsWith(pat + '/');
  });
}

// renderPreviews shows the exact token each path/exclude becomes in the script.
// Unanchored patterns silently reaching outside the source is the mistake this
// is here to make visible (spec §3.3.1).
function renderPreviews() {
  const fill = (ul, values, prefix) => {
    ul.innerHTML = '';
    values.forEach((v) => {
      const li = document.createElement('li');
      li.textContent = prefix + anchorPreview(v);
      ul.appendChild(li);
    });
  };
  fill($('paths_preview'), lines($('paths').value), '');

  const sources = lines($('paths').value);
  const ul = $('excludes_preview');
  ul.innerHTML = '';
  lines($('excludes').value).forEach((pattern) => {
    const li = document.createElement('li');
    li.textContent = '--exclude ' + anchorPreview(pattern);
    if (literalPattern(pattern) && !excludeReaches(pattern, sources)) {
      li.classList.add('inert');
      const note = document.createElement('span');
      note.className = 'inert-note';
      note.textContent = '  ✗ matches nothing under the sources';
      li.appendChild(note);
    }
    ul.appendChild(li);
  });

  const spec = $('compression_preview');
  spec.innerHTML = '';
  const li = document.createElement('li');
  li.textContent = `--compression ${compressionSpec()}`;
  spec.appendChild(li);
}

// lastGenerateOk gates "Git commit & push": committing a job that never
// successfully validated, or one the editor no longer matches (dirty / a
// form change pending regeneration), would push something other than what
// the form currently says.
let lastGenerateOk = false;

// Generation counter, not a mutex: concurrent requests are fine, only their
// *results* must not be applied out of order. Nothing guarantees responses
// come back in the order they were sent, so without this a slower reply from
// an older form state could land after a newer one and leave the editor
// showing a script the form no longer describes. Bumped before the request,
// re-checked after every await and before the first DOM write; a superseded
// run returns silently, having touched nothing. See also gitLoadSeq.
let generateSeq = 0;

// Call before replacing the editor with content that did not come from
// generate() — a library load or a file import. Those set the editor to the
// exact stored/uploaded text on purpose, so a generate() still in flight
// (started by a form edit made moments earlier) must not land afterwards and
// overwrite it with output from the state the form had *before* the load.
// Bumping the counter is enough: the in-flight run fails its own check and
// returns without touching anything.
function supersedePendingGenerate() {
  generateSeq++;
}

async function generate() {
  lastGenerateOk = false;
  const seq = ++generateSeq;
  const payload = {
    job: collect(),
    user_pre: $('runtime_pre').value,
    user_post: $('runtime_post').value,
    filename: filenameBase(),
  };

  try {
    const resp = await fetch('/api/generate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const data = await resp.json();
    if (seq !== generateSeq) return;
    if (!resp.ok) {
      showProblems(data.warnings && data.warnings.length ? data.warnings : [data.error]);
      setStatus('not generated', 'err');
      return;
    }
    showProblems([]);
    Ed.set(data.script);
    syncZoneFields(data.script);
    editorDirty = false;
    formChangedWhileDirty = false;
    renderDirtyState();
    setStatus('generated', 'ok');
    lastGenerateOk = true;
    saveDraft();
    // No dedicated "Git pull" button: Regenerate is the one action pressed
    // often enough to double as "the job picker is due for a refresh."
    if (!$('git_pull_row').hidden) refreshLibraryJobs();
  } catch (e) {
    if (seq !== generateSeq) return;
    setStatus('network error: ' + e.message, 'err');
  }
}

// While the editor holds manual edits, regeneration stops being automatic and
// becomes an explicit action — otherwise changing any field would silently
// discard everything the user typed outside the user zones.
function renderDirtyState() {
  const box = $('dirty');
  const btn = $('btn_regenerate');
  box.classList.toggle('active', editorDirty);
  btn.classList.toggle('pending', formChangedWhileDirty);

  if (formChangedWhileDirty) {
    box.textContent =
      'The form changed, but the editor holds manual edits — press "Regenerate" to apply. ' +
      'Everything outside the borggen:user zones will be replaced.';
  } else if (editorDirty) {
    box.textContent =
      'Manually edited. "Regenerate" keeps the borggen:user zones and replaces everything else.';
  } else {
    box.textContent = '';
  }
}

let draftTimer = null;
function scheduleDraft() {
  if (suppressDraft) return;
  clearTimeout(draftTimer);
  draftTimer = setTimeout(saveDraft, 1200);
}

async function saveDraft() {
  if (suppressDraft) return;
  try {
    await fetch('/api/draft', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ job: collect(), filename: filenameBase(), script_text: Ed.get() }),
    });
  } catch (e) {
    setStatus('draft not saved: ' + e.message, 'err');
  }
}

// filenameBase is what Save/Push name the .sh file — independent of
// BACKUPNAME (job.name): an imported or git-loaded script keeps its real
// filename even when it differs from the job's internal name.
function filenameBase() {
  return $('filename').value.trim() || 'backup';
}

// downloadText triggers a browser download of a .sh file. Force LF: these
// scripts run on Linux, a CRLF shebang line is fatal.
function downloadText(text, filename) {
  const blob = new Blob([text.replace(/\r\n?/g, '\n')], { type: 'text/x-shellscript' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}

// navigator.clipboard only exists in a secure context (HTTPS, or literally
// "localhost") — reached over plain HTTP through a reverse proxy by hostname
// or IP, it is undefined outright, not just permission-denied. document.
// execCommand('copy') is deprecated but still the standard fallback: it has
// no such restriction, working anywhere a real user gesture (this click)
// triggers it.
async function copyToClipboard(text) {
  if (navigator.clipboard) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  try {
    if (!document.execCommand('copy')) throw new Error('execCommand returned false');
  } finally {
    ta.remove();
  }
}

function download() {
  downloadText(Ed.get(), filenameBase() + '.sh');
  flashDone($('btn_download'));
}

// Companion scripts (check, restore-drill) are generated straight from the
// current form state and downloaded on the spot — they carry no meta block,
// so there is nothing to keep in the editor or the draft for them.
async function downloadCompanion(endpoint, suffix, label, btn) {
  try {
    const resp = await fetch(endpoint, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ job: collect(), filename: filenameBase() }),
    });
    const data = await resp.json();
    if (!resp.ok) {
      showProblems(data.warnings && data.warnings.length ? data.warnings : [data.error]);
      setStatus(label + ' not generated', 'err');
      return;
    }
    downloadText(data.script, filenameBase() + '-' + suffix + '.sh');
    setStatus(label + ' downloaded', 'ok');
    flashDone(btn);
  } catch (e) {
    setStatus('network error: ' + e.message, 'err');
  }
}

// stripSh removes a trailing .sh so a real file/library name can become the
// Filename field's value (which is stored and shown without the extension).
const stripSh = (name) => name.replace(/\.sh$/i, '');

async function importFile(file) {
  const text = await file.text();
  supersedePendingGenerate();
  try {
    const resp = await fetch('/api/import', { method: 'POST', body: text });
    const data = await resp.json();
    if (!resp.ok) {
      showProblems([data.error]);
      setStatus('import failed', 'err');
      return;
    }
    apply(data.job);
    // A real, uploaded file's name is authoritative — it stops following
    // BACKUPNAME from here on, same as a git-loaded job.
    $('filename').value = stripSh(file.name);
    filenameTouched = true;
    showProblems(data.warnings || []);
    Ed.set(text);
    syncZoneFields(text);
    editorDirty = false;
    formChangedWhileDirty = false;
    renderDirtyState();
    setStatus('imported ' + file.name, 'ok');
    saveDraft();
    flashDone($('btn_import'));
  } catch (e) {
    setStatus('import error: ' + e.message, 'err');
  }
}

// wireInfo opens explanations in a modal over the page. Inline panels pushed
// the whole form down when expanded, which moved every control below them.
function wireInfo() {
  const modal = $('modal');
  const title = $('modal_title');
  const body = $('modal_body');
  let lastFocus = null;

  const close = () => {
    modal.hidden = true;
    body.innerHTML = '';
    if (lastFocus) lastFocus.focus();
  };

  // Blocks whose meaning depends on a choice get one document per option:
  // a generic "transport" text would describe two modes the user did not pick.
  const variantOf = (key) => {
    switch (key) {
      case 'transport': return $('transport_kind').value;
      case 'repo': return $('repo_kind').value;
      case 'compression': return $('compression_algo').value;
      default: return '';
    }
  };

  const open = (key, label) => {
    const variant = variantOf(key);
    const tpl = (variant && document.getElementById(`doc-${key}-${variant}`))
      || document.getElementById('doc-' + key);
    if (!tpl) return;

    title.textContent = variant ? `${label} — ${variant}` : label;
    body.innerHTML = '';
    body.appendChild(tpl.content.cloneNode(true));
    modal.hidden = false;
    $('modal_close').focus();
  };

  document.querySelectorAll('button.info').forEach((btn) => {
    btn.addEventListener('click', (e) => {
      // Some info buttons sit inside a <summary> (collapsible sections) or a
      // <label> (channel checkboxes) — without this, the click would also
      // toggle the details open/closed, or activate the label's checkbox.
      e.preventDefault();
      e.stopPropagation();

      lastFocus = btn;
      // The legend, summary or label the button sits in names the block.
      const host = btn.closest('legend') || btn.closest('summary') || btn.closest('label');
      const label = btn.dataset.label || (host
        ? host.childNodes[0].textContent.trim().replace(/[\s*]+$/, '')
        : 'Info');
      open(btn.dataset.info, label);
    });
  });

  $('modal_close').addEventListener('click', close);
  // A click on the backdrop, but not inside the dialog, dismisses it.
  modal.addEventListener('click', (e) => {
    if (e.target === modal) close();
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !modal.hidden) close();
  });
}

// Not model.DefaultJob() (a worked example: name "backup", a real-looking
// repo path, /etc as the source — indistinguishable from an actual half-set-up
// job) and not {} (fails validation, so Regenerate right after Reset does
// nothing but show red text). Every required field gets an unmistakably-fake
// stub value instead, so Regenerate always succeeds and there is live
// generated code to look at immediately — archive_template has no "obviously
// fake" form, so it keeps the one value that is always correct rather than a
// stub the user would have to fix before the very first successful generate.
const RESET_JOB = {
  name: 'name-your-backup',
  repo: { path: 'borg-init-your-repo-first' },
  source: { paths: ['path-your-sources'], archive_template: '{name}-{now:%Y-%m-%d_%H:%M}' },
};

// wireReset confirms before discarding the form, the editor, and the saved
// draft — regeneration happens on every field change, so there is no other
// undo for "I want the blank state back".
function wireReset() {
  const modal = $('confirm_modal');
  let lastFocus = null;

  const close = () => {
    modal.hidden = true;
    if (lastFocus) lastFocus.focus();
  };

  $('btn_reset').addEventListener('click', (e) => {
    lastFocus = e.currentTarget;
    modal.hidden = false;
    $('confirm_cancel').focus();
  });

  $('confirm_close').addEventListener('click', close);
  $('confirm_cancel').addEventListener('click', close);
  modal.addEventListener('click', (e) => {
    if (e.target === modal) close();
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !modal.hidden) close();
  });

  $('confirm_reset').addEventListener('click', async () => {
    close();
    try {
      apply(RESET_JOB);
      Ed.set('');
      // Not part of RESET_JOB/apply(): the Before/After fields are their own
      // source of truth for generate(), not derived from the job — clearing
      // them here is the only thing that actually empties them.
      $('runtime_pre').value = '';
      $('runtime_post').value = '';
      if ($('git_jobs')) $('git_jobs').value = '';
      // Back to a blank template: Filename mirrors BACKUPNAME again.
      $('filename').value = RESET_JOB.name;
      filenameTouched = false;
      editorDirty = false;
      formChangedWhileDirty = false;
      renderDirtyState();
      showProblems([]);
      await generate();
      setStatus('reset', 'ok');
    } catch (e) {
      setStatus('reset failed: ' + e.message, 'err');
    }
  });
}

// Same generation counter as generateSeq, for loads from the job picker.
let gitLoadSeq = 0;

// Git integration is entirely opt-in via server config
// (BORGGEN_GIT_REMOTE_URL/BORGGEN_GIT_TOKEN) — the token itself never
// reaches the browser. loadGitConfig() at boot reveals this UI only when
// the server actually has a remote configured; the buttons existing at all
// is the only "is this on" signal, no separate status indicator.
function wireGit() {
  $('btn_git_push').addEventListener('click', async () => {
    if (!lastGenerateOk || editorDirty || formChangedWhileDirty) {
      setStatus('regenerate before pushing — the editor does not match the form', 'err');
      return;
    }
    try {
      const resp = await fetch('/api/library/push', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          job: collect(),
          filename: filenameBase(),
          user_pre: $('runtime_pre').value,
          user_post: $('runtime_post').value,
        }),
      });
      const data = await resp.json();
      if (!resp.ok) {
        setStatus('push failed: ' + data.error, 'err');
        return;
      }
      const n = (data.committed || []).length;
      setStatus(n ? `pushed (${n} file${n === 1 ? '' : 's'} changed)` : 'pushed (nothing to commit)', 'ok');
      flashDone($('btn_git_push'));
      // A first-time push can introduce a job name the picker has never
      // seen before.
      refreshLibraryJobs();
    } catch (e) {
      setStatus('network error: ' + e.message, 'err');
    }
  });

  // Loading a job from the library is the same operation as importing a
  // file — apply the parsed job, set the editor to the exact stored text,
  // and re-sync the Before/After fields from it.
  $('git_jobs').addEventListener('change', async (e) => {
    const name = e.target.value;
    if (!name) return;
    // Switching entries faster than the fetches resolve used to leave the
    // picker showing one job and the form holding another: whichever reply
    // landed last won, not whichever job was picked last. Worse, `name` is
    // captured here, so the stale run also rewrote Filename and saved the
    // wrong job to the draft — leaving the form internally consistent but
    // simply not the job on screen, and surviving a reload.
    const seq = ++gitLoadSeq;
    supersedePendingGenerate();
    try {
      const resp = await fetch('/api/library/' + encodeURIComponent(name));
      const data = await resp.json();
      if (seq !== gitLoadSeq) return;
      if (!resp.ok) {
        showProblems([data.error]);
        setStatus('could not load ' + name, 'err');
        return;
      }
      apply(data.job);
      // The library entry's own name is authoritative, same as a real
      // uploaded file — stops following BACKUPNAME from here on.
      $('filename').value = stripSh(name);
      filenameTouched = true;
      showProblems(data.warnings || []);
      Ed.set(data.script);
      syncZoneFields(data.script);
      editorDirty = false;
      formChangedWhileDirty = false;
      renderDirtyState();
      setStatus('loaded ' + name + ' from the library', 'ok');
      saveDraft();
    } catch (err) {
      if (seq !== gitLoadSeq) return;
      setStatus('network error: ' + err.message, 'err');
    }
  });
}

// loadGitConfig reveals the git UI only when the server actually has a
// remote configured — hidden entirely otherwise, not shown disabled, so a
// deploy with no Gitea remote reads as "this feature doesn't exist here"
// rather than something to puzzle over.
async function loadGitConfig() {
  try {
    const resp = await fetch('/api/config');
    const data = await resp.json();

    // Absent, not disabled: with no BORGMON_URL configured the deployment
    // has no borgmon, and the form must not suggest otherwise.
    if (data.borgmon_enabled) {
      $('monitor_row').hidden = false;
      syncVisibility();
    }

    if (!data.git_enabled) return;
    $('btn_git_push').hidden = false;
    $('git_pull_row').hidden = false;
    refreshLibraryJobs();
  } catch (e) {
    // Git UI just stays hidden — not fatal to the rest of the app.
  }
}

// refreshLibraryJobs quietly (re)pulls from the Gitea remote and refills the
// job picker — there is no dedicated "Git pull" button; this runs once at
// boot (loadGitConfig) and again on every Regenerate instead. Always silent:
// a status message here would race with generate()'s own "generated" /
// "not generated" message. resp.ok is checked before .json() specifically
// because this is also reachable while git is unconfigured (the route does
// not exist then, and a 404 body is plain text, not JSON).
async function refreshLibraryJobs() {
  try {
    const resp = await fetch('/api/library/pull');
    if (!resp.ok) return;
    const data = await resp.json();
    const select = $('git_jobs');
    const current = select.value;
    select.innerHTML = '<option value="">— select a job —</option>';
    (data.files || []).forEach((name) => {
      const opt = document.createElement('option');
      opt.value = name;
      opt.textContent = name;
      select.appendChild(opt);
    });
    if (current && (data.files || []).includes(current)) select.value = current;
  } catch (e) {
    // The picker just keeps whatever it already had.
  }
}

function wire() {
  wireInfo();
  wireReset();
  wireGit();

  // Registered before the generic onFormChange loop below, so this runs
  // first on the checkbox's own 'change' event: the textarea already holds
  // the real value by the time onFormChange's generate() reads it. An empty
  // field only — re-enabling after a customized value was left in place
  // (just hidden, not cleared, on uncheck) must not clobber it.
  $('exclude_if_present_enabled').addEventListener('change', () => {
    if ($('exclude_if_present_enabled').checked && !$('exclude_if_present').value.trim()) {
      $('exclude_if_present').value = '.noborgbackup';
    }
  });

  const onFormChange = () => {
    syncVisibility();
    renderPreviews();
    scheduleDraft();
    if (editorDirty) {
      // Do not clobber manual edits behind the user's back.
      formChangedWhileDirty = true;
      renderDirtyState();
      return;
    }
    generate();
  };

  document.querySelectorAll('input, select, textarea').forEach((el) => {
    // git_jobs is a navigation control, not a job parameter — collect() never
    // reads it. Letting it reach onFormChange started a generate() from the
    // *old* form state in parallel with the load of the new job, and the two
    // raced for the editor: the form ended up showing the job just picked
    // while the editor held the previous one's script. Per-flow sequence
    // counters cannot fix that — they are separate counters, each blind to
    // the other flow. See supersedePendingGenerate.
    if (el.id === 'script' || el.id === 'file_input' || el.id === 'git_jobs') return;
    el.addEventListener('change', onFormChange);
  });

  // Previews should track typing, not wait for blur.
  ['paths', 'excludes', 'compression_level', 'compression_spec'].forEach((id) => {
    $(id).addEventListener('input', renderPreviews);
  });
  // The log placeholder is derived from the job name.
  $('name').addEventListener('input', syncVisibility);
  // Filename mirrors BACKUPNAME live, like a slug following a title, until
  // the user edits Filename directly — then it stops following.
  $('name').addEventListener('input', () => {
    if (!filenameTouched) $('filename').value = $('name').value.trim();
  });
  $('filename').addEventListener('input', () => { filenameTouched = true; });

  // A per-field reveal toggle, not one global switch: each secret input gets
  // its own eye button, so revealing one does not expose the other two.
  document.querySelectorAll('.secret-toggle').forEach((btn) => {
    btn.addEventListener('click', (e) => {
      // The button sits inside a <label>; without this, the click would
      // also forward to (and focus) the wrapped input.
      e.preventDefault();
      e.stopPropagation();

      const input = $(btn.dataset.target);
      const show = input.type === 'password';
      input.type = show ? 'text' : 'password';
      btn.classList.toggle('active', show);
    });
  });

  Ed.onChange = () => {
    editorDirty = true;
    renderDirtyState();
    scheduleDraft();
    // A hand-edit made directly in the script's own zone (old habit, still
    // supported) has to win over whatever the Before/After fields currently
    // hold — otherwise the next Regenerate would silently replace it with
    // stale field content instead of preserving it.
    syncZoneFields(Ed.get());
  };

  $('btn_regenerate').addEventListener('click', async () => {
    await generate();
    if (lastGenerateOk) flashDone($('btn_regenerate'));
  });
  $('btn_download').addEventListener('click', download);
  $('btn_import').addEventListener('click', () => $('file_input').click());
  $('btn_check_script').addEventListener('click', () =>
    downloadCompanion('/api/generate/check', 'check', 'check script', $('btn_check_script')));
  $('btn_restore_drill').addEventListener('click', () =>
    downloadCompanion('/api/generate/restore-drill', 'restore-drill', 'restore drill', $('btn_restore_drill')));
  $('btn_copy').addEventListener('click', async () => {
    try {
      await copyToClipboard(Ed.get());
      setStatus('copied to clipboard', 'ok');
      flashDone($('btn_copy'));
    } catch (e) {
      setStatus('copy failed: ' + e.message, 'err');
    }
  });
  $('file_input').addEventListener('change', (e) => {
    if (e.target.files.length) importFile(e.target.files[0]);
    e.target.value = '';
  });

  const editor = $('script');
  editor.addEventListener('dragover', (e) => e.preventDefault());
  editor.addEventListener('drop', (e) => {
    e.preventDefault();
    if (e.dataTransfer.files.length) importFile(e.dataTransfer.files[0]);
  });
}

async function boot() {
  await initEditor();
  wire();
  loadGitConfig();
  try {
    const resp = await fetch('/api/draft');
    const data = await resp.json();
    // No saved draft (a fresh volume: docker compose down -v && up) must look
    // exactly like pressing Reset — not model.DefaultJob()'s worked example
    // (name "backup", a real-looking repo path, /etc as the source), which
    // /api/draft's own "no draft yet" fallback still returns for callers that
    // do want a realistic example rather than a blank slate.
    apply(data.exists ? data.job : RESET_JOB);
    // A saved filename is a real, previously-set value (typed, imported, or
    // git-loaded) — treat it like import/git-load, not like a fresh
    // template: it stops following BACKUPNAME too. Older drafts saved before
    // this field existed have none, so fall back to mirroring.
    if (data.exists && data.filename) {
      $('filename').value = data.filename;
      filenameTouched = true;
    } else {
      $('filename').value = data.exists ? data.job.name : RESET_JOB.name;
      filenameTouched = false;
    }
    if (data.exists && data.script_text) {
      Ed.set(data.script_text);
      syncZoneFields(data.script_text);
      setStatus('draft restored from ' + new Date(data.updated_at).toLocaleString());
    }
  } catch (e) {
    setStatus('could not load draft: ' + e.message, 'err');
  }
  suppressDraft = false;
  generate();
}

boot();
