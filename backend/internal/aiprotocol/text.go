package aiprotocol

import "strings"

// SourceNamePattern preserves the established safe text fallback for source
// and configuration names. Parsing still validates the actual byte encoding.
const SourceNamePattern = `(^|/)(dockerfile|makefile|cmakelists[.]txt|go[.]mod|go[.]sum|package[.]json|package-lock[.]json)$|[.](c|cc|cpp|cxx|h|hh|hpp|hxx|go|java|kt|kts|py|rb|php|rs|swift|js|jsx|mjs|cjs|ts|tsx|vue|svelte|sh|bash|zsh|fish|ps1|sql|graphql|html|htm|css|scss|less|xml|yaml|yml|toml|ini|conf|properties|env)$`

func BinaryMediaType(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	return mime == "application/octet-stream" || mime == "binary/octet-stream" || mime == ""
}

func SupportedText(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	return strings.HasPrefix(mime, "text/") || strings.Contains(mime, "pdf") || strings.Contains(mime, "wordprocessingml") || strings.Contains(mime, "spreadsheetml") || strings.Contains(mime, "json") || strings.Contains(mime, "yaml") || strings.Contains(mime, "csv") || strings.Contains(mime, "javascript") || strings.Contains(mime, "typescript") || mime == "application/sql" || mime == "application/x-sh" || strings.Contains(mime, "xml")
}

// VisionImage is deliberately limited to the formats accepted and bounded by
// the existing vision adapter. A thumbnail alone does not make a source text.
func VisionImage(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	return mime == "image/png" || mime == "image/jpeg"
}

func SupportedSource(mime string) bool { return SupportedText(mime) || VisionImage(mime) }
