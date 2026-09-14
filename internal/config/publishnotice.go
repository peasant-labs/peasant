package config

// PublishConsentPhrase returns the exact consent-control phrase for a license
// choice, as a person reads it at the moment they authorize a publish. The
// phrasing is the wording the privacy notice quotes, in lowercase because it is
// terminal chrome.
//
//	LicenseCCBY   -> "publish under CC BY 4.0"
//	LicenseCCBYSA -> "publish under CC BY-SA 4.0"
//	LicenseCC0    -> "publish under CC0 1.0"
//	""            -> "publish without a license"
//
// The non-empty keys are the closed set schema.AllLicenses. Any OTHER non-empty
// value returns "" by design, so a caller can detect a license the phrasing does
// not yet cover rather than print a wrong or blank line. The exhaustiveness test
// iterates schema.AllLicenses and fails until every member has a phrase, so a
// license added to the contract cannot ship here unmapped.
func PublishConsentPhrase(license License) string {
	switch license {
	case LicenseCCBY:
		return "publish under CC BY 4.0"
	case LicenseCCBYSA:
		return "publish under CC BY-SA 4.0"
	case LicenseCC0:
		return "publish under CC0 1.0"
	case "":
		return "publish without a license"
	default:
		return ""
	}
}
