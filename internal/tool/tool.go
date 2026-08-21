// Package tool resolves external binaries by absolute path.
//
// sudo replaces PATH with secure_path, and /usr/sbin — home of losetup,
// dmsetup and mkfs.btrfs on Debian/Ubuntu — is off a normal non-root PATH.
// Resolving through exec.LookPath alone therefore false-negatives depending
// on who is running, which is how the mkfs.btrfs probe in doctor broke once
// already. Every shell-out goes through here so a new call site cannot get
// it wrong.
package tool

import (
	"fmt"
	"os"
	"os/exec"
)

// sbinDirs are searched after PATH. /usr/local/sbin precedes /usr/sbin to
// match the conventions both Debian and sudo's secure_path use.
var sbinDirs = []string{"/usr/local/sbin", "/usr/sbin", "/sbin"}

// Resolve finds name on PATH, falling back to the usual sbin locations that
// non-root PATHs and sudo's secure_path omit. It returns an absolute path
// suitable for exec, or an error naming what was tried.
func Resolve(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	for _, dir := range sbinDirs {
		path := dir + "/" + name
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s not found on PATH or in %s", name, sbinDirs)
}
