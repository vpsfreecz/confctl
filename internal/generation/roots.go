package generation

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
)

func SafeHost(host string) string { return strings.ReplaceAll(host, "/", ":") }
func configurationPath(configuration string) (string, error) {
	// Resolve components before Abs can lexically collapse link/..; ConfDir
	// identifies the physical configuration directory, not its spelling.
	resolved, err := filepath.EvalSymlinks(configuration)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

type RootIdentity struct {
	Configuration, Hash, ShortHash, Login, Directory string
}

// ComputeRootIdentity receives the login identity from the owning adapter.
// In particular it does not substitute USER, os/user.Current or a UID when
// Etc.getlogin is absent. It only resolves the configuration path and hashes it.
func ComputeRootIdentity(configuration string, login *string) (RootIdentity, error) {
	if login == nil {
		return RootIdentity{}, fmt.Errorf("login identity is missing")
	}
	real, err := configurationPath(configuration)
	if err != nil {
		return RootIdentity{}, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(real)))
	return RootIdentity{Configuration: real, Hash: hash, ShortHash: hash[:8], Login: *login, Directory: filepath.Join("/nix/var/nix/gcroots/per-user", *login, "confctl-"+hash[:8])}, nil
}
func RootName(host, generation, component string) string {
	return SafeHost(host) + "-generation-" + generation + "-" + component
}
