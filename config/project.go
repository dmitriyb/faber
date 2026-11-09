package config

import (
	"crypto/sha256"
	"encoding/hex"
)

// ProjectID names a project by its config file: the first 12 hex digits of
// the SHA-256 of the file's identity (symlinks resolved, absolute — the
// loader's own notion, so every spelling of one path is one project). It
// prefixes the project's docker volumes, so every run of one project shares
// them and two projects never collide by accident.
func ProjectID(configPath string) string {
	sum := sha256.Sum256([]byte(identity(configPath)))
	return hex.EncodeToString(sum[:])[:12]
}

// VolumePrefix is what every docker volume of a project starts with: the
// filter `faber volumes` lists and prunes by.
func VolumePrefix(project string) string { return "faber-" + project + "-" }

// VolumeName is the docker name of a template's named volume.
func VolumeName(project, name string) string { return VolumePrefix(project) + name }
