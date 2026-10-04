package outbound

import anytlsutil "github.com/anytls/sing-anytls/util"

// sing-box v1.12.21 pins sing-anytls v0.0.11, which unconditionally puts
// "sing-anytls/<version>" into the AnyTLS settings frame (`client` field).
// Some providers use that field to identify and reject clients built on the
// official library, and the field plays no role in protocol negotiation, so
// Resin blanks it before any outbound session is created.
//
// Adopted from upstream PR #88. Remove this shim once the sing-box/sing-anytls
// dependency is upgraded to a version that defaults the client metadata to an
// empty string. Note: v0.0.13 renamed the exported symbol from the historical
// misspelling "Verison" to "Version", so this shim must be updated (or dropped)
// alongside any dependency bump.
func init() {
	anytlsutil.Verison = ""
}
