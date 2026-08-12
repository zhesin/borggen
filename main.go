// Command borggen serves the backup script constructor.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"borggen/internal/draft"
	"borggen/internal/httpapi"
	"borggen/internal/library"
)

//go:embed frontend
var frontendFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dataDir := flag.String("data", "/data", "directory for the draft and the git-backed library")
	flag.Parse()

	front, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		log.Fatalf("frontend: %v", err)
	}

	drafts, err := draft.NewStore(*dataDir + "/draft.json")
	if err != nil {
		log.Fatalf("draft store: %v", err)
	}

	git, err := setupGitRemote(*dataDir)
	if err != nil {
		log.Fatalf("git remote: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		// BORGMON_URL enables the monitoring feature. Unset, the UI never
		// mentions it and no generated script can reference it — the same
		// absent-rather-than-disabled rule the git integration follows.
		Handler:           httpapi.New(drafts, front, git, strings.TrimSpace(os.Getenv("BORGMON_URL"))),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Shut down on the same signals the generated scripts handle, so a
	// container stop does not cut an in-flight request.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("borggen listening on %s, data dir %s", *addr, *dataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Print("borggen stopped")
}

// setupGitRemote wires the git-backed library to a Gitea remote from
// environment variables — never CLI flags, so the token never shows up in
// `ps aux`. Both must be set together, or git integration stays off: the
// frontend hides the whole feature rather than showing it disabled, so a
// partially configured remote (bad URL, no token) would just be a silent
// dead end otherwise.
func setupGitRemote(dataDir string) (*httpapi.GitRemote, error) {
	url := os.Getenv("BORGGEN_GIT_REMOTE_URL")
	token := os.Getenv("BORGGEN_GIT_TOKEN")
	switch {
	case url == "" && token == "":
		return nil, nil
	case url == "" || token == "":
		log.Print("warning: only one of BORGGEN_GIT_REMOTE_URL/BORGGEN_GIT_TOKEN is set — git integration stays disabled")
		return nil, nil
	}

	lib, err := library.Open(filepath.Join(dataDir, "library"))
	if err != nil {
		return nil, err
	}
	return &httpapi.GitRemote{Library: lib, RemoteURL: url, Token: token}, nil
}
