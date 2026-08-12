// Package httpapi exposes the generator, the importer and the draft store.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"

	"borggen/internal/draft"
	"borggen/internal/generator"
	"borggen/internal/importer"
	"borggen/internal/model"
)

// maxScriptBytes caps uploads; generated scripts are a few kilobytes.
const maxScriptBytes = 1 << 20

// gitLibrary is the subset of *library.Library the HTTP layer needs — an
// interface so handlers can be tested against a fake instead of a real git
// repository and a live Gitea remote. *library.Library satisfies it as-is.
type gitLibrary interface {
	Save(filename, content string) (bool, error)
	Push(url, token string) error
	Pull(url, token string) error
	List() ([]string, error)
	Read(filename string) (string, error)
}

// GitRemote bundles the local library with the Gitea remote it publishes to.
// A nil *GitRemote passed to New means git integration is not configured:
// the /api/library/* endpoints are not registered at all (a 404, not a
// disabled-but-present feature) and /api/config reports it as off so the
// frontend hides the git UI entirely rather than showing it greyed out.
type GitRemote struct {
	Library   gitLibrary
	RemoteURL string
	Token     string
}

// Server wires the handlers to their dependencies.
type Server struct {
	drafts   *draft.Store
	frontend fs.FS
	git      *GitRemote
	// monitorURL is the borgmon base URL, from the environment. Empty means
	// the feature does not exist here: the UI never mentions it, and no job
	// can be configured to push. Same hide-entirely-when-unconfigured rule
	// the git integration follows.
	monitorURL string
}

// New builds the HTTP handler. git may be nil — see GitRemote.
func New(drafts *draft.Store, frontend fs.FS, git *GitRemote, monitorURL string) http.Handler {
	s := &Server{drafts: drafts, frontend: frontend, git: git, monitorURL: monitorURL}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/generate", s.handleGenerate)
	mux.HandleFunc("POST /api/generate/check", s.handleGenerateCheck)
	mux.HandleFunc("POST /api/generate/restore-drill", s.handleGenerateRestoreDrill)
	mux.HandleFunc("POST /api/import", s.handleImport)
	mux.HandleFunc("GET /api/draft", s.handleDraftGet)
	mux.HandleFunc("PUT /api/draft", s.handleDraftPut)
	mux.HandleFunc("GET /api/defaults", s.handleDefaults)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	if git != nil {
		mux.HandleFunc("POST /api/library/push", s.handleLibraryPush)
		mux.HandleFunc("GET /api/library/pull", s.handleLibraryPull)
		mux.HandleFunc("GET /api/library/{name}", s.handleLibraryGet)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.Handle("/", http.FileServerFS(frontend))
	return mux
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	resp := map[string]any{
		"git_enabled":     s.git != nil,
		"borgmon_enabled": s.monitorURL != "",
	}
	if s.git != nil {
		resp["git_remote_url"] = s.git.RemoteURL
	}
	if s.monitorURL != "" {
		resp["borgmon_url"] = s.monitorURL
	}
	writeJSON(w, http.StatusOK, resp)
}

type generateRequest struct {
	Job      model.BackupJob `json:"job"`
	UserPre  string          `json:"user_pre"`
	UserPost string          `json:"user_post"`
	// Filename is the on-disk base name, independent of Job.Name/BACKUPNAME:
	// handleLibraryPush saves/pushes the file under it, and every generate
	// endpoint derives LOG_FILE/LOCK_FILE from it (generator.Options.Filename)
	// — see effectiveBase. Empty falls back to Job.Name.
	Filename string `json:"filename,omitempty"`
}

// effectiveBase is the on-disk identity LOG_FILE/LOCK_FILE and the companion
// filenames are always derived from: the request's Filename when given, else
// Job.Name — the same fallback the frontend's filename-mirrors-BACKUPNAME
// behavior already uses, kept in sync here server-side. Never overridable:
// there is no form field or model.BackupJob field for either path, on
// purpose — see CLAUDE.md.
func effectiveBase(filename, jobName string) string {
	if f := strings.TrimSpace(filename); f != "" {
		return f
	}
	return jobName
}

type generateResponse struct {
	Script   string   `json:"script"`
	Warnings []string `json:"warnings"`
}

func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	var req generateRequest
	if !decode(w, r, &req) {
		return
	}

	req.Job.Normalize()
	if errs := req.Job.Validate(); len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":    "invalid parameters",
			"warnings": errs,
		})
		return
	}

	script, err := generator.Generate(req.Job, generator.Options{
		MonitorURL: s.monitorURL,
		UserPre:    req.UserPre,
		UserPost: req.UserPost,
		Filename: effectiveBase(req.Filename, req.Job.Name),
	})
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, generateResponse{Script: script})
}

// companionRequest carries the job and Filename: check/restore-drill scripts
// have no hand-edit zones and are not round-tripped through /api/import, so
// there is no user_pre/user_post to pass through, but they still need
// Filename to derive their own LOG_FILE/LOCK_FILE default (see
// effectiveBase).
type companionRequest struct {
	Job      model.BackupJob `json:"job"`
	Filename string          `json:"filename,omitempty"`
}

func (s *Server) handleGenerateCheck(w http.ResponseWriter, r *http.Request) {
	handleCompanion(w, r, generator.GenerateCheckScript)
}

func (s *Server) handleGenerateRestoreDrill(w http.ResponseWriter, r *http.Request) {
	handleCompanion(w, r, generator.GenerateRestoreDrillScript)
}

func handleCompanion(w http.ResponseWriter, r *http.Request, gen func(model.BackupJob, generator.Options) (string, error)) {
	var req companionRequest
	if !decode(w, r, &req) {
		return
	}

	base := effectiveBase(req.Filename, req.Job.Name)
	req.Job.Normalize()
	if errs := req.Job.Validate(); len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":    "invalid parameters",
			"warnings": errs,
		})
		return
	}

	// No MonitorURL: a companion never pushes to borgmon, for the same reason
	// it never pushes the uptime-kuma heartbeat — a monthly check reporting
	// itself as a run of the backup job would be a second, confusing source
	// of truth for "is this job alive".
	script, err := gen(req.Job, generator.Options{Filename: base})
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, generateResponse{Script: script})
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxScriptBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "could not read request body"})
		return
	}

	res, err := importer.Parse(string(body))
	if err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, importer.ErrNoMarker) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDraftGet(w http.ResponseWriter, _ *http.Request) {
	st, ok, err := s.drafts.Load()
	if err != nil {
		log.Printf("draft load: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not read draft"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"exists": false, "job": model.DefaultJob()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exists":      true,
		"job":         st.Job,
		"filename":    st.Filename,
		"script_text": st.ScriptText,
		"updated_at":  st.UpdatedAt,
	})
}

func (s *Server) handleDraftPut(w http.ResponseWriter, r *http.Request) {
	var st draft.State
	if !decode(w, r, &st) {
		return
	}
	if err := s.drafts.Save(st); err != nil {
		log.Printf("draft save: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not save draft"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDefaults(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, model.DefaultJob())
}

// handleLibraryPush commits the current job (and its check/restore-drill
// companions) to the local library and publishes all of it in one push —
// three files, at most three commits, one network round trip. Registered
// only when git is configured (New checks this), so s.git is never nil here.
func (s *Server) handleLibraryPush(w http.ResponseWriter, r *http.Request) {
	var req generateRequest
	if !decode(w, r, &req) {
		return
	}
	filename := effectiveBase(req.Filename, req.Job.Name)
	req.Job.Normalize()
	if errs := req.Job.Validate(); len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":    "invalid parameters",
			"warnings": errs,
		})
		return
	}

	main, err := generator.Generate(req.Job, generator.Options{UserPre: req.UserPre, UserPost: req.UserPost, Filename: filename, MonitorURL: s.monitorURL})
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	check, err := generator.GenerateCheckScript(req.Job, generator.Options{Filename: filename, MonitorURL: s.monitorURL})
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	drill, err := generator.GenerateRestoreDrillScript(req.Job, generator.Options{Filename: filename, MonitorURL: s.monitorURL})
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}

	files := map[string]string{
		filename + ".sh": main,
		filename + "-" + generator.CheckSuffix + ".sh":        check,
		filename + "-" + generator.RestoreDrillSuffix + ".sh": drill,
	}
	committed := make([]string, 0, len(files))
	for name, content := range files {
		did, err := s.git.Library.Save(name, content)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if did {
			committed = append(committed, name)
		}
	}

	if err := s.git.Library.Push(s.git.RemoteURL, s.git.Token); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":     err.Error(),
			"committed": committed,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"committed": committed, "pushed": true})
}

// handleLibraryPull fetches from the Gitea remote and returns the resulting
// list of job files (companion scripts filtered out — they carry no meta
// block, so there is nothing for the form to load from one). A pull failure
// (network, auth) does not fail the request: the local listing is still
// returned, with the failure surfaced as a warning instead of blocking the
// user from seeing what is already available offline.
func (s *Server) handleLibraryPull(w http.ResponseWriter, _ *http.Request) {
	var warning string
	if err := s.git.Library.Pull(s.git.RemoteURL, s.git.Token); err != nil {
		warning = err.Error()
	}

	all, err := s.git.Library.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jobs := make([]string, 0, len(all))
	for _, name := range all {
		if isCompanionFile(name) {
			continue
		}
		jobs = append(jobs, name)
	}

	resp := map[string]any{"files": jobs}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleLibraryGet returns a library job file parsed the same way
// /api/import does, so the frontend can reuse its existing apply() path
// regardless of whether the script came from disk or from the library —
// plus the raw script text, which /api/import does not need to echo back
// (the frontend already has it, having just uploaded it) but this endpoint
// does, so the editor can be set to the exact stored text.
func (s *Server) handleLibraryGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	content, err := s.git.Library.Read(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	res, err := importer.Parse(content)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job":       res.Job,
		"user_pre":  res.UserPre,
		"user_post": res.UserPost,
		"warnings":  res.Warnings,
		"script":    content,
	})
}

func isCompanionFile(name string) bool {
	return strings.HasSuffix(name, "-"+generator.CheckSuffix+".sh") ||
		strings.HasSuffix(name, "-"+generator.RestoreDrillSuffix+".sh")
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxScriptBytes))
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("write response: %v", err)
	}
}
