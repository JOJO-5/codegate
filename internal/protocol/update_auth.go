package protocol

import (
	"strconv"
	"strings"
)

// UpdateSigningPayload binds an Agent download request to its HTTP host, path
// and timestamp. Both sides must use the URL host seen by the Agent.
func UpdateSigningPayload(host, path, timestamp string) []byte {
	return []byte("codegate-update-v1\nGET\n" + host + "\n" + path + "\n" + timestamp)
}

// ReleaseSigningPayload excludes hosting URLs so artifacts may be mirrored
// without access to the offline signing key. All executable identity fields
// are bound to the signature, including size, platform and version.
func ReleaseSigningPayload(version, goos, arch, digest string, size int64) []byte {
	return []byte("codegate-release-v1\n" + version + "\n" + goos + "\n" + arch + "\n" + strings.ToLower(digest) + "\n" + strconv.FormatInt(size, 10))
}
