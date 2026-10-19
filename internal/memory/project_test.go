package memory

import (
	"strings"
	"testing"
)

func TestSanitizeProject(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"piproxy", "piproxy", false},
		{"PiProxy", "piproxy", false},
		{"my project (v2)", "my-project-v2", false},
		{"repo.name_ok", "repo.name_ok", false},
		{"../../etc/passwd", "etc-passwd", false}, // separators collapse to dashes
		{"..", "", true},
		{".", "", true},
		{"", "", true},
		{"   ", "", true},
		{"///", "", true},
		{"%2e%2e", "2e-2e", false},
		{strings.Repeat("a", 100), "", true},
		{"Ünïcødé Pröject", "n-c-d-pr-ject", false},
	}

	for _, tt := range tests {
		got, err := SanitizeProject(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("SanitizeProject(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("SanitizeProject(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("SanitizeProject(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if strings.ContainsAny(got, "/\\") || strings.Contains(got, "..") {
			t.Errorf("SanitizeProject(%q) = %q contains path separators or traversal", tt.in, got)
		}
	}
}

func TestMemoryPaths(t *testing.T) {
	if FilePath("demo") != "projects/demo/memory.md" {
		t.Error("FilePath wrong")
	}
	if LessonsFilePath("demo") != "projects/demo/lessons.md" {
		t.Error("LessonsFilePath wrong")
	}
	if PromptReflectionsFilePath("demo") != "projects/demo/prompt-reflections.md" {
		t.Error("PromptReflectionsFilePath wrong")
	}
}
