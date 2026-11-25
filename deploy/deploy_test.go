package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestEnvironmentExamplesCoverRequiredSettings(t *testing.T) {
	required := []string{"PORT", "DB_PATH", "UPLOAD_DIR", "SEARCH_INDEX_DIR", "ATTACHMENT_DIR", "SITE_NAME"}
	for _, name := range []string{"dev", "staging", "production"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("environments", name+".env.example")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			values := make(map[string]string)
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				key, value, ok := strings.Cut(line, "=")
				if ok {
					values[key] = value
				}
			}
			for _, key := range required {
				if values[key] == "" {
					t.Errorf("%s missing required setting %s", path, key)
				}
			}
		})
	}
}

func TestSystemdUnitsHaveSafeServiceSettings(t *testing.T) {
	for _, name := range []string{"sorus.service", "sorus-staging.service"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("systemd", name))
			if err != nil {
				t.Fatal(err)
			}
			content := string(data)
			for _, setting := range []string{
				"[Service]", "ExecStart=/usr/local/bin/sorus", "KillSignal=SIGTERM",
				"StateDirectory=", "ProtectSystem=strict", "ProtectHome=yes", "NoNewPrivileges=yes",
				"PrivateTmp=yes", "PrivateDevices=yes", "RestrictAddressFamilies=",
			} {
				if !strings.Contains(content, setting) {
					t.Errorf("%s missing %q", name, setting)
				}
			}
		})
	}
	backup, err := os.ReadFile(filepath.Join("systemd", "sorus-backup.service"))
	if err != nil {
		t.Fatal(err)
	}
	timer, err := os.ReadFile(filepath.Join("systemd", "sorus-backup.timer"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backup), "ExecStart=/usr/local/bin/sorus backup") {
		t.Error("backup service does not invoke the online backup command")
	}
	if !strings.Contains(string(timer), "OnCalendar=*-*-* 03:00:00 Asia/Tokyo") {
		t.Error("backup timer is not scheduled for 03:00 JST")
	}
}

func TestComposeFilesParseAndHaveExpectedStructure(t *testing.T) {
	for _, tc := range []struct {
		file, service, volume, port string
	}{
		{"docker-compose.yml", "sorus", "sorus-prod-data", "80:80"},
		{"docker-compose.staging.yml", "sorus-staging", "sorus-staging-data", "8081:8080"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := yaml.Unmarshal(data, &doc); err != nil {
				t.Fatalf("parse YAML: %v", err)
			}
			services, ok := doc["services"].(map[string]any)
			if !ok {
				t.Fatal("services mapping missing")
			}
			service, ok := services[tc.service].(map[string]any)
			if !ok {
				t.Fatalf("service %q missing", tc.service)
			}
			volumes, ok := doc["volumes"].(map[string]any)
			if _, exists := volumes[tc.volume]; !ok || !exists {
				t.Fatalf("volume %q missing", tc.volume)
			}
			if service["restart"] != "unless-stopped" {
				t.Errorf("restart = %v, want unless-stopped", service["restart"])
			}
			if _, ok := service["healthcheck"].(map[string]any); !ok {
				t.Error("healthcheck missing")
			}
			ports, ok := service["ports"].([]any)
			if !ok || !containsString(ports, tc.port) {
				t.Errorf("ports %v do not include %q", service["ports"], tc.port)
			}
			if tc.file == "docker-compose.yml" && containsString(ports, "8080:8080") {
				t.Errorf("production compose file should not expose plain HTTP port 8080 publicly")
			}
		})
	}
}

func TestMainUsesENVFileBeforeDefaultDotEnv(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `loadEnvFile(envOr("ENV_FILE", ".env"))`) {
		t.Fatal("main does not prefer ENV_FILE with .env as fallback")
	}
}

func containsString(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
