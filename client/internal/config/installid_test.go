package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

func TestInstallIDIsMadeOnceAndKept(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	t.Cleanup(SetUserConfigDirForTest(dir))

	// Act
	first := InstallID()
	second := InstallID()

	// Assert
	if wire.ValidInstallID(first) == "" {
		t.Fatalf("not an install id: %q", first)
	}
	if second != first {
		t.Fatalf("the id changed between calls: %q then %q", first, second)
	}
	info, err := os.Stat(filepath.Join(dir, "leoprevent", UserInstallIDFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the id file is %v, want 0600", info.Mode().Perm())
	}
}

func TestInstallIDDiffersBetweenInstallations(t *testing.T) {
	// Arrange
	restore := SetUserConfigDirForTest(t.TempDir())
	a := InstallID()
	restore()
	t.Cleanup(SetUserConfigDirForTest(t.TempDir()))

	// Act
	b := InstallID()

	// Assert
	if a == "" || b == "" || a == b {
		t.Fatalf("two installations got %q and %q", a, b)
	}
}

func TestInstallIDConcurrentHooksAgree(t *testing.T) {
	// Arrange
	t.Cleanup(SetUserConfigDirForTest(t.TempDir()))
	const hooks = 16
	ids := make([]string, hooks)
	var wg sync.WaitGroup

	// Act
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i] = InstallID()
		}()
	}
	wg.Wait()

	// Assert
	for i, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("hook %d got %q, hook 0 got %q", i, id, ids[0])
		}
	}
}

func TestInstallIDLeavesADamagedFileAlone(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	t.Cleanup(SetUserConfigDirForTest(dir))
	path := filepath.Join(dir, "leoprevent", UserInstallIDFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not-an-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Act
	id := InstallID()

	// Assert
	if id != "" {
		t.Fatalf("a damaged file produced the id %q", id)
	}
	if data, _ := os.ReadFile(path); string(data) != "not-an-id\n" {
		t.Fatalf("the damaged file was rewritten: %q", data)
	}
}

func TestInstallIDWithoutAConfigDir(t *testing.T) {
	// Arrange
	old := userConfigDir
	userConfigDir = func() (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { userConfigDir = old })

	// Act
	id := InstallID()

	// Assert
	if id != "" {
		t.Fatalf("no config dir produced the id %q", id)
	}
}
