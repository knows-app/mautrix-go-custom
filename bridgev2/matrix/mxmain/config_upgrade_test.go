package mxmain

import (
	"strings"
	"testing"

	"go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
)

// upgradeAndParse runs a user config through the same upgrade + parse path
// LoadConfig uses at bridge startup.
func upgradeAndParse(t *testing.T, userConfig string) (string, bridgeconfig.Config) {
	t.Helper()
	var buf strings.Builder
	err := matrixExampleConfigBaseTemplate.Execute(&buf, bridgev2.BridgeName{
		DisplayName: "Test", NetworkID: "test", NetworkIcon: "mxc://test/icon", DefaultPort: 29999,
	})
	if err != nil {
		t.Fatal(err)
	}
	upgrader := configupgrade.MergeUpgraders("network: {}\n"+buf.String(), bridgeconfig.Upgrader)
	out, err := configupgrade.DoBytes([]byte(userConfig), upgrader)
	if err != nil {
		t.Fatalf("upgrade failed: %v", err)
	}
	var cfg bridgeconfig.Config
	if err = yaml.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("Failed to parse config: %v\nupgraded config:\n%s", err, out)
	}
	return string(out), cfg
}

func TestLoginAdmissionSurvivesConfigUpgrade(t *testing.T) {
	_, cfg := upgradeAndParse(t, `
bridge:
    command_prefix: '!meta'
    login_admission:
        url: http://callbacks.example.internal:8080/internal/bridge/customer-login/admit
        platform: messenger
        shard_id: fb01
        allow_insecure_http: true
`)
	la := cfg.Bridge.LoginAdmission
	if la == nil {
		t.Fatal("login_admission was dropped by the upgrade")
	}
	if la.URL != "http://callbacks.example.internal:8080/internal/bridge/customer-login/admit" ||
		la.Platform != "messenger" || la.ShardID != "fb01" || !la.AllowInsecureHTTP || la.Secret != "" {
		t.Fatalf("unexpected login_admission after upgrade: %+v", *la)
	}
	if cfg.Bridge.CommandPrefix != "!meta" {
		t.Fatalf("neighbouring key lost: %q", cfg.Bridge.CommandPrefix)
	}
}

func TestLoginAdmissionUpgradeIsStable(t *testing.T) {
	first, _ := upgradeAndParse(t, `
bridge:
    login_admission: {url: "https://cb.example/admit", platform: instagram, shard_id: ig01}
`)
	// The upgraded file is saved and re-read on the next start.
	_, cfg := upgradeAndParse(t, first)
	if la := cfg.Bridge.LoginAdmission; la == nil || la.URL != "https://cb.example/admit" ||
		la.Platform != "instagram" || la.ShardID != "ig01" {
		t.Fatalf("login_admission did not survive a second upgrade: %+v", la)
	}
}

func TestLoginAdmissionStaysOffWhenUnset(t *testing.T) {
	for name, userConfig := range map[string]string{
		"absent":   "bridge:\n    command_prefix: '!meta'\n",
		"null":     "bridge:\n    login_admission: null\n",
		"wrongtyp": "bridge:\n    login_admission: yes\n",
	} {
		t.Run(name, func(t *testing.T) {
			out, cfg := upgradeAndParse(t, userConfig)
			if cfg.Bridge.LoginAdmission != nil {
				t.Fatalf("login_admission should stay off, got %+v", *cfg.Bridge.LoginAdmission)
			}
			if !strings.Contains(out, "login_admission: null") {
				t.Fatalf("expected the null default to be kept:\n%s", out)
			}
		})
	}
}
