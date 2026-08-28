package main

import (
	"path"
	"path/filepath"
	"strings"
)

// Secret guard: filename and path patterns only. No content scanning by design.
// Patterns without a slash match the file name; patterns with a slash match the
// end of the repo-relative path. Matching is case-insensitive.
var secretPatterns = []string{
	// environment files
	".env", ".env.*", "*.env",
	// private keys and key stores
	"id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*",
	"*.key", "*.pem", "*.p8", "*.p12", "*.pfx", "*.ppk", "*.jks", "*.keystore", "*.kdbx",
	// tool credentials
	".npmrc", ".pypirc", ".netrc", ".git-credentials", ".htpasswd", ".pgpass", ".my.cnf", ".vault-token", "auth.json",
	// cloud tools
	".aws/credentials", "application_default_credentials.json", ".config/gh/hosts.yml",
	".docker/config.json", ".kube/config", "*.kubeconfig",
	// infrastructure
	"*.tfstate*", "*.tfvars", ".terraform.d/credentials.tfrc.json",
	// generic secret files
	"secrets.json", "secrets.yml", "secrets.yaml",
	"credentials.json", "credentials.yml", "credentials.yaml",
	"client_secret*.json", "service-account*.json",
}

// Templates are safe to publish even though they match ".env.*".
var secretTemplates = []string{".env.example", ".env.sample", ".env.template", ".env.dist"}

// isSecretPath reports whether a repo-relative path is blocked. Entries in
// allow are repo-relative paths, directories, or globs, and always win.
func isSecretPath(rel string, allow []string) bool {
	rel = normalizeRel(rel)
	base := path.Base(rel)
	for _, entry := range allow {
		entry = normalizeRel(entry)
		if entry == rel || strings.HasPrefix(rel, entry+"/") {
			return false
		}
		if ok, _ := path.Match(entry, rel); ok {
			return false
		}
	}
	for _, template := range secretTemplates {
		if base == template {
			return false
		}
	}
	for _, pattern := range secretPatterns {
		if strings.Contains(pattern, "/") {
			if rel == pattern || strings.HasSuffix(rel, "/"+pattern) {
				return true
			}
			continue
		}
		if ok, _ := path.Match(pattern, base); ok {
			return true
		}
	}
	return false
}

func normalizeRel(value string) string {
	return strings.ToLower(path.Clean(filepath.ToSlash(value)))
}
