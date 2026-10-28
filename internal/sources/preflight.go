package sources

import (
	"bytes"

	"samuori.invalid/samuori/internal/secretscan"
)

// Warning codes are stable identifiers; messages never contain the matched content.
const (
	WarnPathRejected    = "path.rejected"
	WarnLinkRejected    = "entry.link-rejected"
	WarnSpecialRejected = "entry.special-rejected"
	WarnSecretExcluded  = "secret.file-excluded"
	WarnPrivateKey      = "secret.private-key"
	WarnCloudKey        = "secret.cloud-access-key"
	WarnToken           = "secret.token"
	WarnExecutable      = "binary.executable"
	WarnMalwareTest     = "malware.test-signature"
	WarnTruncated       = "warnings.truncated"
)

type Warning struct {
	Code, Message, Path string
}

const contentScanBytes = 1 << 20

// The EICAR string is split so this source file is not itself flagged by antivirus tools.
var eicar = []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}$` + `EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`)

var executableMagic = [][]byte{
	{0x7f, 'E', 'L', 'F'},
	{'M', 'Z'},
	{0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf}, {0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe},
	{0xca, 0xfe, 0xba, 0xbe},
}

// scanContent applies best-effort checks to the first contentScanBytes of a file. It cannot prove absence.
func scanContent(p string, head []byte) []Warning {
	var out []Warning
	for _, m := range executableMagic {
		if bytes.HasPrefix(head, m) && !(m[0] == 'M' && len(head) < 64) && !(m[0] == 0xca && !looksLikeMachOFat(head)) {
			out = append(out, Warning{WarnExecutable, "Contains a compiled executable. Builds normally produce binaries from source; check it is expected.", p})
			break
		}
	}
	if bytes.Contains(head, eicar) {
		out = append(out, Warning{WarnMalwareTest, "Contains the EICAR antivirus test signature.", p})
	}
	for _, r := range secretscan.Match(head) {
		out = append(out, Warning{r.Code, r.Message, p})
	}
	return out
}

// looksLikeMachOFat distinguishes fat Mach-O binaries from Java class files, which share the 0xCAFEBABE magic.
func looksLikeMachOFat(head []byte) bool {
	return len(head) >= 8 && head[4] == 0 && head[5] == 0 && head[6] == 0 && head[7] > 0 && head[7] < 20
}
