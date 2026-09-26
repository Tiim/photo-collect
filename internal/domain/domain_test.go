package domain

import (
	"slices"
	"testing"
)

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Alice":               "alice",
		"  Bob  The Builder ": "bob-the-builder",
		"José Ñandú":          "jose-nandu",
		"../../etc/passwd":    "etc-passwd",
		"<script>x</script>":  "script-x-script",
		"日本語":                 "anonymous",
		"":                    "anonymous",
		"a/b":                 "a-b",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	if got := UploaderTag("Alice B"); got != "uploader/alice-b" {
		t.Errorf("UploaderTag = %q", got)
	}
}

func TestUniqueNames(t *testing.T) {
	got := UniqueNames([]string{"IMG_1234.jpg", "IMG_1234.jpg", "img_1234.JPG", "other.png", "IMG_1234.jpg"})
	want := []string{"IMG_1234.jpg", "IMG_1234_2.jpg", "img_1234_3.JPG", "other.png", "IMG_1234_4.jpg"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestSafeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd": "passwd",
		`C:\Users\x\a.jpg`: "a.jpg",
		"a\x00b.jpg":       "ab.jpg",
		"":                 "image",
		"..":               "image",
		`we"ird?.jpg`:      "weird.jpg",
	} {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeTag(t *testing.T) {
	if got, err := NormalizeTag("  Team   RED "); err != nil || got != "team red" {
		t.Fatalf("got %q %v", got, err)
	}
	for _, bad := range []string{"", "   ", "a,b", "<b>"} {
		if _, err := NormalizeTag(bad); err == nil {
			t.Errorf("NormalizeTag(%q) should fail", bad)
		}
	}
}
