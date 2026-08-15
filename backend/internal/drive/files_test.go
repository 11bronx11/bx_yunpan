package drive

import "testing"

func TestPreviewKindRejectsScriptableImages(t *testing.T) {
	tests := map[string]string{
		"image/jpeg":      "image",
		"image/svg+xml":   "unsupported",
		"text/html":       "text",
		"application/pdf": "pdf",
	}
	for mimeType, expected := range tests {
		if actual := previewKind(mimeType); actual != expected {
			t.Fatalf("MIME type %q: expected %q, got %q", mimeType, expected, actual)
		}
	}
}
