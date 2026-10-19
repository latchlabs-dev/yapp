package memory

import "fmt"

// PurgatoryFilePath keeps unresolved observations outside projects/, so neither
// project discovery/compaction nor tenant-scoped hint search can adopt them.
// The claimed project label is metadata only, never an ownership/storage key.
func PurgatoryFilePath(tenantID, userID, keyID string) (string, error) {
	tenant, err := SanitizeProject(tenantID)
	if err != nil {
		return "", fmt.Errorf("invalid purgatory tenant: %w", err)
	}
	ownerType, ownerID := "user", userID
	if ownerID == "" {
		ownerType, ownerID = "key", keyID
	}
	owner, err := SanitizeProject(ownerID)
	if err != nil {
		return "", fmt.Errorf("invalid purgatory owner: %w", err)
	}
	return "purgatory/" + tenant + "/" + ownerType + "-" + owner + "/observations.md", nil
}
