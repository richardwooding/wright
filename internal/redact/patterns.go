package redact

import "regexp"

// Default pattern names, exported so callers can disable them by name.
const (
	NamePrivateKey      = "private-key"
	NameAnthropic       = "anthropic"
	NameOpenAI          = "openai"
	NameGitHub          = "github"
	NameGitHubPAT       = "github-pat"
	NameAWSAccessKey    = "aws-access-key"
	NameAWSSecretKey    = "aws-secret-key"
	NameGoogleAPIKey    = "google-api-key"
	NameGCPPrivateKeyID = "gcp-private-key-id"
	NameSlack           = "slack"
	NameStripe          = "stripe"
	NameNPM             = "npm"
	NamePyPI            = "pypi"
	NameHuggingFace     = "huggingface"
	NameJWT             = "jwt"
	NameAuthHeader      = "auth-header"
	NameURLPassword     = "url-password"
	NameGeneric         = "generic"
)

// defaults are applied in order; vendor-prefixed patterns run before the
// broader ones so a token is attributed to the most specific name.
var defaults = []Pattern{
	{Name: NamePrivateKey, Re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{Name: NameAnthropic, Re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`), Keep: 4},
	{Name: NameOpenAI, Re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`), Keep: 4},
	{Name: NameGitHub, Re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,255}`), Keep: 4},
	{Name: NameGitHubPAT, Re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,255}`), Keep: 4},
	{Name: NameAWSAccessKey, Re: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), Keep: 4},
	{Name: NameAWSSecretKey, Re: regexp.MustCompile(`(?i)aws[_-]?secret[_-]?access[_-]?key["']?\s*[:=]\s*["']?([A-Za-z0-9/+=]{40})\b`), Group: 1, Keep: 4},
	{Name: NameGoogleAPIKey, Re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), Keep: 4},
	{Name: NameGCPPrivateKeyID, Re: regexp.MustCompile(`"private_key_id"\s*:\s*"([a-f0-9]{40})"`), Group: 1, Keep: 4},
	{Name: NameSlack, Re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,255}`), Keep: 4},
	{Name: NameStripe, Re: regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,255}`), Keep: 4},
	{Name: NameNPM, Re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`), Keep: 4},
	{Name: NamePyPI, Re: regexp.MustCompile(`\bpypi-AgEIcHlwaS5vcmc[A-Za-z0-9_-]{20,}`), Keep: 4},
	{Name: NameHuggingFace, Re: regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,255}\b`), Keep: 4},
	{Name: NameJWT, Re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), Keep: 4},
	{Name: NameAuthHeader, Re: regexp.MustCompile(`(?i)(authorization\s*:\s*(?:bearer|basic|token)\s+)([A-Za-z0-9._~+/=-]{8,})`), Group: 2, Keep: 4},
	{Name: NameURLPassword, Re: regexp.MustCompile(`://[^\s/:@\[\]]+:([^\s/@\[\]]+)@`), Group: 1},
}

// genericPattern is the opt-in assignment pattern; apply() adds the entropy
// gate so ordinary words assigned to a *_TOKEN variable are left alone.
var genericPattern = Pattern{
	Name:  NameGeneric,
	Re:    regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:secret|token|password|passwd|api[_-]?key|private[_-]?key|access[_-]?key)[A-Z0-9_]*)["']?\s*[:=]\s*["']?([A-Za-z0-9+/=_.~-]{16,})`),
	Group: 2,
	Keep:  4,
}

// Defaults returns a copy of the default pattern list.
func Defaults() []Pattern {
	out := make([]Pattern, len(defaults))
	copy(out, defaults)
	return out
}
