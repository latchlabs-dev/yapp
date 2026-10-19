package postgres

import "testing"

func TestNormalizeRepositoryRemote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"https", "https://github.com/Example-Org/Example-App.git", "github.com/example-org/example-app"},
		{"scp ssh", "git@github.com:example-org/example-app.git", "github.com/example-org/example-app"},
		{"ssh url", "ssh://git@github.com/example-org/example-app.git", "github.com/example-org/example-app"},
		{"credentials omitted", "https://user:secret@gitlab.com/example-org/example-service.git?x=1", "gitlab.com/example-org/example-service"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeRepositoryRemote(tt.raw)
			if err != nil {
				t.Fatalf("NormalizeRepositoryRemote: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeRepositoryRemoteRejectsLocalPaths(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "/code/demo", "demo", "file:///code/demo"} {
		if got, err := NormalizeRepositoryRemote(raw); err == nil {
			t.Errorf("NormalizeRepositoryRemote(%q) = %q, want error", raw, got)
		}
	}
}

func TestDecodeProjectParticipantsBuildsUnambiguousInsightUsers(t *testing.T) {
	project := &AdminMemoryProject{}
	err := decodeProjectParticipants(project, []byte(`[
		{"user_id":"usr_a","display_name":"A","email":"a@example.com","session_count":2,"message_count":5,"total_cost_cents":1.2,"conversation_ids":["conv-a","shared"]},
		{"user_id":"usr_b","display_name":"B","email":"b@example.com","session_count":1,"message_count":3,"total_cost_cents":0.8,"conversation_ids":["conv-b","shared"]}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(project.Participants) != 2 {
		t.Fatalf("participants = %+v", project.Participants)
	}
	if got := project.InsightUser("conversation", "conv-a"); got == nil || got.ID != "usr_a" {
		t.Fatalf("conv-a user = %+v", got)
	}
	if got := project.InsightUser("user", "usr_b"); got == nil || got.Email != "b@example.com" {
		t.Fatalf("usr_b = %+v", got)
	}
	if got := project.InsightUser("conversation", "shared"); got != nil {
		t.Fatalf("ambiguous conversation resolved to %+v", got)
	}
}
