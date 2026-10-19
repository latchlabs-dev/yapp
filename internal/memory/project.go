// Package memory implements duffel-backed project memory for the proxy:
// context bundle assembly (injected into Pi sessions at start), an injection
// hook chain (programmatic + DB-rule gating), session distillation, and
// memory compaction.
package memory

import (
	"fmt"
	"regexp"
	"strings"
)

const maxProjectNameLength = 64

var projectInvalidChars = regexp.MustCompile(`[^a-z0-9._-]+`)
var projectDashRuns = regexp.MustCompile(`-{2,}`)

// SanitizeProject normalizes a client-supplied project identifier into a safe
// duffel path segment. It is the single authority on project naming: every
// duffel path the proxy touches is built from its output, which can never
// contain path separators or traversal sequences.
func SanitizeProject(raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	name = projectInvalidChars.ReplaceAllString(name, "-")
	name = projectDashRuns.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-.")

	if name == "" {
		return "", fmt.Errorf("project name is empty after sanitization")
	}
	if len(name) > maxProjectNameLength {
		return "", fmt.Errorf("project name exceeds %d characters", maxProjectNameLength)
	}
	// Defense-in-depth: the character class above already excludes '/' and
	// the trim removes leading dots, but keep explicit traversal checks.
	if name == "." || name == ".." || strings.Contains(name, "..") {
		return "", fmt.Errorf("project name %q is not allowed", name)
	}
	return name, nil
}

// ScopedProject returns the on-disk project key for one tenant. Project
// memory is never shared solely by repository basename because unrelated
// tenants commonly use identical names such as "api" or "website".
func ScopedProject(tenantID, project string) (string, error) {
	tenant, err := SanitizeProject(tenantID)
	if err != nil {
		return "", fmt.Errorf("invalid tenant scope: %w", err)
	}
	project, err = SanitizeProject(project)
	if err != nil {
		return "", err
	}
	return tenant + "--" + project, nil
}

// FilePath returns the duffel path of a project's curated memory file.
func FilePath(project string) string {
	return "projects/" + project + "/memory.md"
}

// LessonsFilePath returns the duffel path of a project's lessons journal.
func LessonsFilePath(project string) string {
	return "projects/" + project + "/lessons.md"
}

// PromptReflectionsFilePath returns the duffel path of a project's prompt
// quality journal. Prompting feedback is intentionally kept out of lessons.md
// so it can be reviewed, retained, or exported independently.
func PromptReflectionsFilePath(project string) string {
	return "projects/" + project + "/prompt-reflections.md"
}
