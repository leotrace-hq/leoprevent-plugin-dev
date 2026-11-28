package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

// UserInstallIDFile holds this installation's id, beside the license file.
const UserInstallIDFile = "install-id"

// InstallID is this installation's id (wire.TurnMeta.InstallID): 16 random bytes in hex, made up
// the first time it is asked for and kept in <UserConfigDir>/leoprevent/install-id. One per OS
// user account on one computer, which is what the agent inventory (LEO-822) is kept by.
//
// ⚠️ RANDOM, AND DERIVED FROM NOTHING. Not the hostname, a hardware serial, the MAC address, the
// user name or the license key: any of those would say something about the machine or the person,
// and this only has to tell one installation from another. Deleting the file gives the
// installation a new id, and the server then sees a new computer.
//
// In the config dir, not the scratch (cache) dir: a cache is cleared by the OS and by clean-up
// tools, and an id that changes makes one computer look like many.
//
// "" on any failure (no config dir, a read-only disk, a file that is not an id). The caller then
// sends none; the inventory is analytics and never fails a turn.
func InstallID() string {
	dir, err := userConfigDir()
	if err != nil {
		return ""
	}
	dir = filepath.Join(dir, "leoprevent")
	path := filepath.Join(dir, UserInstallIDFile)
	if id, err := readInstallID(path); err == nil {
		return id
	} else if !errors.Is(err, fs.ErrNotExist) {
		// There, but not an id (truncated, edited, a directory). Left alone: replacing it would
		// hide whatever happened to it, and the cost of leaving it is only that no id is sent.
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	var raw [wire.InstallIDLen / 2]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ""
	}
	id := hex.EncodeToString(raw[:])
	// Written whole to a file of its own, then LINKED into place. A link fails when the name is
	// taken, so of two hooks starting together one wins and the other reads the winner's id; and
	// the name never exists half-written, which a create-then-write would let a third hook read
	// as "not an id".
	tmp, err := os.CreateTemp(dir, UserInstallIDFile+".*")
	if err != nil {
		return ""
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.WriteString(id + "\n")
	if cerr := tmp.Close(); werr != nil || cerr != nil {
		return ""
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if id, err := readInstallID(path); err == nil {
				return id
			}
		}
		return ""
	}
	return id
}

// readInstallID reads the id file. fs.ErrNotExist when there is none; another error when what is
// there is not an id.
func readInstallID(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 2*wire.InstallIDLen {
		return "", errors.New("install id: not a small regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := wire.ValidInstallID(strings.TrimSpace(string(data)))
	if id == "" {
		return "", errors.New("install id: not an id")
	}
	return id, nil
}
