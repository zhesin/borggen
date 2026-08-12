// Package draft stores the current, unsaved form state.
//
// The draft is technical autosave, not the library: one file, no git, no
// naming, its only job is that work is not lost when the browser or the device
// changes.
package draft

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"borggen/internal/model"
)

// State is the persisted draft.
type State struct {
	Job model.BackupJob `json:"job"`
	// Filename names the .sh file on Save/Push — independent of Job.Name
	// (BACKUPNAME): an imported or library-loaded script keeps its real
	// filename here even when that differs from the job's internal name.
	Filename   string    `json:"filename,omitempty"`
	ScriptText string    `json:"script_text"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Store is a single-file JSON store with atomic writes.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore prepares the store, creating the parent directory if needed.
func NewStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("draft directory: %w", err)
	}
	return &Store{path: path}, nil
}

// Load returns the stored draft. A missing file is not an error: it means the
// user has not typed anything yet.
func (s *Store) Load() (State, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("read draft: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, false, fmt.Errorf("parse draft: %w", err)
	}
	return st, true, nil
}

// Save writes the draft atomically: a concurrent autosave must never be able
// to leave a half-written file behind.
func (s *Store) Save(st State) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	st.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal draft: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".draft-*.tmp")
	if err != nil {
		return fmt.Errorf("draft temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write draft: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync draft: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close draft: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("rename draft: %w", err)
	}
	return nil
}
