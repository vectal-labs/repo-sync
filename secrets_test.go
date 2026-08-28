package main

import "testing"

func TestIsSecretPath(t *testing.T) {
	blocked := []string{
		".env", ".env.local", ".ENV.production", "config/.env", "app.env",
		"id_rsa", "id_rsa.pub", "id_ed25519", "keys/server.key", "cert.PEM", "AuthKey_ABC.p8",
		"a.p12", "a.pfx", "a.ppk", "a.jks", "release.keystore", "vault.kdbx",
		".npmrc", "sub/.pypirc", ".netrc", ".git-credentials", ".htpasswd", ".pgpass", ".my.cnf", ".vault-token", "auth.json",
		".aws/credentials", "home/.aws/credentials", "gcloud/application_default_credentials.json",
		".config/gh/hosts.yml", ".docker/config.json", ".kube/config", "prod.kubeconfig",
		"terraform.tfstate", "terraform.tfstate.backup", "prod.tfvars", ".terraform.d/credentials.tfrc.json",
		"secrets.json", "secrets.yml", "conf/secrets.yaml", "credentials.json", "credentials.yml", "credentials.yaml",
		"client_secret_123.json", "service-account-prod.json",
	}
	for _, path := range blocked {
		if !isSecretPath(path, nil) {
			t.Errorf("%q should be blocked", path)
		}
	}
	allowed := []string{
		".env.example", ".env.sample", ".env.template", ".env.dist", "config/.ENV.example",
		"README.md", "main.go", "environment.md", "my-secret-plan.md", "token.go", "credentials.md",
		"config.json", "hosts.yml", "docker/config.json", "kube/config.yaml", "keys.txt", "envelope",
	}
	for _, path := range allowed {
		if isSecretPath(path, nil) {
			t.Errorf("%q should not be blocked", path)
		}
	}
}

func TestIsSecretPathOverrides(t *testing.T) {
	if isSecretPath(".env", []string{".env"}) {
		t.Error("exact allow entry must win")
	}
	if isSecretPath("config/.env", []string{"config"}) {
		t.Error("directory allow entry must cover its files")
	}
	if isSecretPath("certs/dev.pem", []string{"certs/*.pem"}) {
		t.Error("glob allow entry must win")
	}
	if !isSecretPath("certs/dev.pem", []string{".env"}) {
		t.Error("unrelated allow entry must not unblock")
	}
	if isSecretPath("Config/.ENV", []string{"config/.env"}) {
		t.Error("allow matching must be case-insensitive")
	}
}
