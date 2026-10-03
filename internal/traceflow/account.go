package traceflow

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func accountReference(secret []byte, identity usage.AccountIdentity, family string) (string, string) {
	if len(secret) == 0 || identity.Family() != family || identity.ProviderKey() == "" {
		return "unknown", ""
	}
	var coverage string
	var parts []string
	switch {
	case family == "codex" && identity.WorkspaceID() != "" && identity.MemberID() != "":
		coverage, parts = "provider-account", []string{"provider-account/1", "codex", identity.WorkspaceID(), identity.MemberID()}
	case family == "claude" && identity.OrganizationUUID() != "" && identity.AccountUUID() != "":
		coverage, parts = "provider-account", []string{"provider-account/1", "claude", identity.OrganizationUUID(), identity.AccountUUID()}
	case identity.Family() == family && family != "" && identity.ProviderKey() != "" && identity.AuthID() != "":
		coverage, parts = "credential", []string{"credential/1", family, identity.ProviderKey(), identity.AuthID()}
	default:
		return "unknown", ""
	}
	id, err := jsArray(parts)
	if err != nil {
		return "unknown", ""
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(coverage))
	mac.Write([]byte{0})
	mac.Write([]byte(id))
	return coverage, hex.EncodeToString(mac.Sum(nil))
}

// jsArray follows the exact JSON.stringify string escaping required by the shared identity contract.
func jsArray(parts []string) (string, error) {
	var b strings.Builder
	b.WriteByte('[')
	for i, part := range parts {
		if part == "" || !utf8.ValidString(part) {
			return "", errors.New("missing account identity evidence")
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for _, r := range part {
			switch r {
			case '"', '\\':
				b.WriteByte('\\')
				b.WriteRune(r)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				if r < 0x20 {
					const digits = "0123456789abcdef"
					b.WriteString(`\u00`)
					b.WriteByte(digits[byte(r)>>4])
					b.WriteByte(digits[byte(r)&15])
				} else {
					b.WriteRune(r)
				}
			}
		}
		b.WriteByte('"')
	}
	b.WriteByte(']')
	return b.String(), nil
}

func bindingID(secret []byte, endpoint, apiKey string) string {
	keyMac := hmac.New(sha256.New, secret)
	keyMac.Write([]byte("cliproxyapi.traceflow.binding-key/1"))
	mac := hmac.New(sha256.New, keyMac.Sum(nil))
	for _, field := range []string{"binding/1", endpoint, apiKey} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		mac.Write(length[:])
		mac.Write([]byte(field))
	}
	return hex.EncodeToString(mac.Sum(nil))
}
