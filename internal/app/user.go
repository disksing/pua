package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/disksing/pua/internal/workspacepath"
)

const (
	// LegacyDefaultUserName identifies the user created by older PUA versions.
	// It is retained for migration only; new Workspaces do not create it.
	LegacyDefaultUserName = "User"
	// DefaultUserName remains as a source-compatible alias for integrations
	// that refer to the historical name. PUA no longer creates or selects it.
	DefaultUserName  = LegacyDefaultUserName
	userNameMaxBytes = 80
)

var ErrLastUser = errors.New("cannot delete the last Workspace user")

// UserProfile is the Workspace-local identity and preference record.
// User names are identifiers rather than display labels in the first version.
type UserProfile struct {
	Version    int    `json:"version"`
	Name       string `json:"name"`
	Preference string `json:"preference"`
}

// reservedUserName reports whether name collides with a stable PUA resource
// ID (workspace, scheduler, projectN, projectN.taskM) or a lookalike word.
// User names share the `pua message send --to=` target namespace with
// resource IDs, so these names would make a send target ambiguous.
func reservedUserName(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "workspace", "scheduler", "project", "task":
		return true
	}
	for _, prefix := range []string{"project", "task"} {
		if digits, ok := strings.CutPrefix(lower, prefix); ok && digits != "" && allASCIIDigits(digits) {
			return true
		}
	}
	return false
}

func allASCIIDigits(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return len(value) > 0
}

func ValidateUserName(name string) error {
	if name == "" {
		return errors.New("user name is required")
	}
	if len(name) > userNameMaxBytes {
		return fmt.Errorf("user name must be at most %d characters", userNameMaxBytes)
	}
	for _, value := range name {
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '_' || value == '-' {
			continue
		}
		return errors.New("user name may contain only letters, numbers, underscores, and hyphens")
	}
	if reservedUserName(name) {
		return fmt.Errorf("user name %q is reserved for PUA resource addressing", name)
	}
	return nil
}

func usersPath(root string) string {
	return filepath.Join(workspacepath.ControlDir(root), "users")
}

func userPath(root, name string) string {
	return filepath.Join(usersPath(root), name)
}

func userProfilePath(root, name string) string {
	return filepath.Join(userPath(root, name), "profile.json")
}

func ensureUserLocked(root, name string) (UserProfile, bool, error) {
	if err := ValidateUserName(name); err != nil {
		return UserProfile{}, false, err
	}
	path := userProfilePath(root, name)
	var profile UserProfile
	if err := readJSON(path, &profile); err == nil {
		if profile.Name != name {
			return UserProfile{}, false, fmt.Errorf("user profile name %q does not match directory %q", profile.Name, name)
		}
		profile.Version = 1
		return profile, false, nil
	} else if !os.IsNotExist(err) {
		return UserProfile{}, false, err
	}
	profile = UserProfile{Version: 1, Name: name}
	if err := writeJSON(path, profile); err != nil {
		return UserProfile{}, false, err
	}
	return profile, true, nil
}

func (w *Workspace) RegisterUser(name string) (profile UserProfile, err error) {
	if err := w.require(); err != nil {
		return UserProfile{}, err
	}
	if err := ValidateUserName(name); err != nil {
		return UserProfile{}, &APIError{Operation: "register User", Kind: "user", Workspace: w.root, Err: err}
	}
	err = withWorkspaceMutationLock(w.root, func() error {
		var inner error
		profile, _, inner = ensureUserLocked(w.root, name)
		return inner
	})
	if err != nil {
		return UserProfile{}, &APIError{Operation: "register User", Kind: "user", Workspace: w.root, Path: name, Err: err}
	}
	return profile, nil
}

func (w *Workspace) User(name string) (UserProfile, error) {
	if err := w.require(); err != nil {
		return UserProfile{}, err
	}
	if err := ValidateUserName(name); err != nil {
		return UserProfile{}, &APIError{Operation: "read User", Kind: "user", Workspace: w.root, Err: err}
	}
	var profile UserProfile
	if err := readJSON(userProfilePath(w.root, name), &profile); err != nil {
		if os.IsNotExist(err) {
			err = fmt.Errorf("user not found: %s", name)
		}
		return UserProfile{}, &APIError{Operation: "read User", Kind: "user", Workspace: w.root, Path: name, Err: err}
	}
	if profile.Name != name {
		return UserProfile{}, &APIError{Operation: "read User", Kind: "user", Workspace: w.root, Path: name, Err: fmt.Errorf("profile name %q does not match", profile.Name)}
	}
	profile.Version = 1
	return profile, nil
}

func (w *Workspace) Users() ([]UserProfile, error) {
	if err := w.require(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(usersPath(w.root))
	if os.IsNotExist(err) {
		return []UserProfile{}, nil
	}
	if err != nil {
		return nil, &APIError{Operation: "list Users", Kind: "user", Workspace: w.root, Err: err}
	}
	profiles := make([]UserProfile, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || ValidateUserName(entry.Name()) != nil {
			continue
		}
		profile, readErr := w.User(entry.Name())
		if readErr != nil {
			return nil, readErr
		}
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

func (w *Workspace) UpdateUserPreference(name, preference string) (profile UserProfile, err error) {
	if err := w.require(); err != nil {
		return UserProfile{}, err
	}
	if err := ValidateUserName(name); err != nil {
		return UserProfile{}, &APIError{Operation: "update User", Kind: "user", Workspace: w.root, Err: err}
	}
	err = withWorkspaceMutationLock(w.root, func() error {
		var readErr error
		profile, readErr = w.User(name)
		if readErr != nil {
			return readErr
		}
		profile.Version = 1
		profile.Preference = strings.TrimSpace(preference)
		return writeJSON(userProfilePath(w.root, name), profile)
	})
	if err != nil {
		return UserProfile{}, &APIError{Operation: "update User", Kind: "user", Workspace: w.root, Path: name, Err: err}
	}
	return profile, nil
}

func (w *Workspace) DeleteUser(name string) error {
	if err := w.require(); err != nil {
		return err
	}
	if err := ValidateUserName(name); err != nil {
		return &APIError{Operation: "delete User", Kind: "user", Workspace: w.root, Err: err}
	}
	err := withWorkspaceMutationLock(w.root, func() error {
		path := userPath(w.root, name)
		info, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			return fmt.Errorf("user not found: %s", name)
		}
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("user path is not a directory: %s", name)
		}
		users, usersErr := w.Users()
		if usersErr != nil {
			return usersErr
		}
		if len(users) <= 1 {
			return ErrLastUser
		}
		return os.RemoveAll(path)
	})
	if err != nil {
		return &APIError{Operation: "delete User", Kind: "user", Workspace: w.root, Path: name, Err: err}
	}
	return nil
}
