package config

import (
	"strings"
	"testing"
)

func TestSessionArchiveDefaultsArePostgresAndValid(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	sa := cfg.SessionArchive
	if !sa.Enabled || sa.ObjectsBackend != SessionObjectsPostgres || sa.RawBodies != SessionRawBodiesPostgres || sa.MergeSegments != 32 || sa.ObjectsPrefix != "piproxy" {
		t.Fatalf("unexpected defaults: %+v", sa)
	}
	if sa.RawBodiesInObjects() || !sa.RawBodiesInPostgres() {
		t.Fatalf("postgres default must keep raw bodies in postgres only: %+v", sa)
	}
}

func TestSessionArchiveGCSDefaultsRawBodiesToObjects(t *testing.T) {
	t.Setenv("PROXY_SESSION_OBJECTS_BACKEND", "GCS")
	t.Setenv("PROXY_SESSION_OBJECTS_BUCKET", "piproxy-sessions")
	t.Setenv("PROXY_SESSION_OBJECTS_PREFIX", "/prod/")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	sa := cfg.SessionArchive
	if sa.ObjectsBackend != SessionObjectsGCS || sa.RawBodies != SessionRawBodiesObjects || sa.ObjectsPrefix != "prod" {
		t.Fatalf("unexpected gcs settings: %+v", sa)
	}
	if !sa.RawBodiesInObjects() || sa.RawBodiesInPostgres() {
		t.Fatalf("objects mode must not also write postgres bodies: %+v", sa)
	}

	t.Setenv("PROXY_SESSION_RAW_BODIES", "both")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SessionArchive.RawBodiesInObjects() || !cfg.SessionArchive.RawBodiesInPostgres() {
		t.Fatalf("both mode must write both: %+v", cfg.SessionArchive)
	}
}

func TestSessionArchiveValidationRejectsInconsistentSettings(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"unknown backend", map[string]string{"PROXY_SESSION_OBJECTS_BACKEND": "s3"}, "unsupported PROXY_SESSION_OBJECTS_BACKEND"},
		{"gcs without bucket", map[string]string{"PROXY_SESSION_OBJECTS_BACKEND": "gcs"}, "PROXY_SESSION_OBJECTS_BUCKET is required"},
		{"objects without gcs", map[string]string{"PROXY_SESSION_RAW_BODIES": "objects"}, "requires PROXY_SESSION_OBJECTS_BACKEND=gcs"},
		{"unknown raw placement", map[string]string{"PROXY_SESSION_RAW_BODIES": "s3"}, "unsupported PROXY_SESSION_RAW_BODIES"},
		{"merge too small", map[string]string{"PROXY_SESSION_ARCHIVE_MERGE_SEGMENTS": "1"}, "at least 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestSessionQueryConfiguration(t *testing.T) {
	t.Setenv("PROXY_SESSION_QUERY_URL", "http://session-query:8092")
	if _, err := Load(); err == nil {
		t.Fatal("service URL without token accepted")
	}
	t.Setenv("PROXY_SESSION_QUERY_TOKEN", strings.Repeat("x", 32))
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"file:///etc/passwd", "http://token@host", "http://host?key=secret", "http://host#fragment"} {
		t.Setenv("PROXY_SESSION_QUERY_URL", value)
		if _, err := Load(); err == nil {
			t.Fatalf("invalid service URL accepted: %s", value)
		}
	}
}
