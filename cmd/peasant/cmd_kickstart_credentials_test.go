package main

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/defaults"
)

func TestKickstartLoginUsesCommandCredentialStore(t *testing.T) {
	defaultHome := t.TempDir()
	customHome := t.TempDir()
	t.Setenv(defaults.EnvXDGConfigHome.String(), defaultHome)
	defaultCredentials := &auth.Credentials{
		APIKey: "default-key", KeyID: "default-key-id", UserID: "default-user-id", Username: "default-user",
	}
	if err := auth.SaveCredentials(defaultCredentials); err != nil {
		t.Fatalf("seed default credentials: %v", err)
	}

	root := newTestRoot()
	command := buildKickstartCommand(defaultKickstartCommandDeps())
	root.AddCommand(command)
	if err := root.PersistentFlags().Set("config-dir", customHome); err != nil {
		t.Fatalf("set custom config dir: %v", err)
	}
	login := kickstartLoginFunc(command, defaults.ResolveConfigFilePathWith(customHome).String())
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := login(cancelled, nil); err == nil || !strings.Contains(err.Error(), "login cancelled") || strings.Contains(err.Error(), "default-user") {
		t.Fatalf("custom kickstart login checked default credentials: %v", err)
	}
	if villageAlreadyConnected(customHome) {
		t.Fatal("custom kickstart profile inherited default-profile connection state")
	}

	customCredentials := &auth.Credentials{
		APIKey: "custom-key", KeyID: "custom-key-id", UserID: "custom-user-id", Username: "custom-user",
	}
	if err := auth.SaveCredentialsFrom(customCredentials, customHome); err != nil {
		t.Fatalf("seed custom credentials: %v", err)
	}
	if !villageAlreadyConnected(customHome) {
		t.Fatal("custom kickstart profile did not detect its own credentials")
	}
	if _, err := login(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "already logged in as custom-user") {
		t.Fatalf("custom kickstart login did not check selected-profile identity: %v", err)
	}
	loadedDefault, err := auth.LoadCredentials()
	if err != nil || loadedDefault == nil || loadedDefault.Username != defaultCredentials.Username {
		t.Fatalf("custom kickstart checks changed default credentials = %#v, %v", loadedDefault, err)
	}
}
