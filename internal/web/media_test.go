package web

import "testing"

func TestSanitizeMime(t *testing.T) {
	cases := map[string]string{
		"":                         "application/octet-stream",
		"image/jpeg":               "image/jpeg",
		"image/png; charset=utf-8": "image/png",
		"text/html":                "application/octet-stream", // blocked
		"application/xhtml+xml":    "application/octet-stream", // blocked
		"image/svg+xml":            "application/octet-stream", // blocked (XSS via SVG)
		"text/javascript":          "application/octet-stream", // blocked
		"application/javascript":   "application/octet-stream", // blocked
		"text/ecmascript":          "application/octet-stream", // blocked
		"application/ecmascript":   "application/octet-stream", // blocked
		"image/jpeg<script>":       "application/octet-stream", // bad chars
		"weird":                    "application/octet-stream", // no slash
		"/leading":                 "application/octet-stream",
		"trailing/":                "application/octet-stream",
		"application/vnd.api+json": "application/vnd.api+json",
		"image/webp":               "image/webp",
	}
	for in, want := range cases {
		if got := sanitizeMime(in); got != want {
			t.Errorf("sanitizeMime(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"foo.png":          "foo.png",
		"../../etc/passwd": "passwd",
		"foo/bar/baz.txt":  "baz.txt",
		"weird\nname.txt":  "weirdname.txt",
		`bad"quote"name`:   "badquotename",
		"..":               "",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
