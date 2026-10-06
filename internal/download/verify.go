package download

import "strings"

// VerifySHA256 checks a downloaded file against a published hex sha256 digest
// (and size, when > 0) -- the same gate the HuggingFace path applies to models,
// exported for the Claude Code release download. A "sha256:" prefix is accepted.
func VerifySHA256(path, wantHex string, size int64) error {
	return verifyFileSHA256(path, strings.TrimPrefix(strings.TrimSpace(wantHex), "sha256:"), size)
}
