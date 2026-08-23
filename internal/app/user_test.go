package app_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestWorkspaceUsersLifecycleAndValidation(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	users, err := workspace.Users()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Fatalf("initialized users = %#v, want none", users)
	}

	if _, err := workspace.RegisterUser("alice_2-test"); err != nil {
		t.Fatal(err)
	}
	profile, err := workspace.UpdateUserPreference("alice_2-test", "  concise replies  ")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Preference != "concise replies" {
		t.Fatalf("preference = %q", profile.Preference)
	}
	users, err = workspace.Users()
	if err != nil {
		t.Fatal(err)
	}
	gotNames := []string{users[0].Name}
	if !reflect.DeepEqual(gotNames, []string{"alice_2-test"}) {
		t.Fatalf("user names = %v", gotNames)
	}
	if _, err := os.Stat(filepath.Join(workspace.Root(), ".pua", "users", "alice_2-test", "profile.json")); err != nil {
		t.Fatal(err)
	}
	if err := workspace.DeleteUser("alice_2-test"); err != nil {
		if !errors.Is(err, app.ErrLastUser) {
			t.Fatal(err)
		}
	}
	if _, err := workspace.RegisterUser("Bob"); err != nil {
		t.Fatal(err)
	}
	if err := workspace.DeleteUser("alice_2-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.User("alice_2-test"); err == nil {
		t.Fatal("deleted user remains readable")
	}

	for _, name := range []string{"", "two words", "../escape", "中文", "name.dot"} {
		if err := app.ValidateUserName(name); err == nil {
			t.Fatalf("invalid user name %q was accepted", name)
		}
	}
	for _, name := range []string{"workspace", "Workspace", "scheduler", "SCHEDULER", "project", "task", "project1", "task42", "Project0", "TASK7"} {
		if err := app.ValidateUserName(name); err == nil {
			t.Fatalf("reserved user name %q was accepted", name)
		}
	}
	for _, name := range []string{"User", "disksing", "project-manager", "task_runner", "projection", "taskforce"} {
		if err := app.ValidateUserName(name); err != nil {
			t.Fatalf("valid user name %q was rejected: %v", name, err)
		}
	}
}
