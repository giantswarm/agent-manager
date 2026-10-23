package oci

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// LoadDockerConfig reads the registry credentials of a Docker config file,
// the .dockerconfigjson of a kubernetes.io/dockerconfigjson Secret: each host
// of auths with its auth (base64 of username:password), else its username and
// password. A host may be written as a URL (https://host/v1/); its host is
// the key.
func LoadDockerConfig(path string) (map[string]Basic, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator's mounted Secret
	if err != nil {
		return nil, fmt.Errorf("read registry config: %w", err)
	}
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("registry config %s: not a Docker config file: %w", path, err)
	}
	out := make(map[string]Basic, len(cfg.Auths))
	for key, a := range cfg.Auths {
		host := dockerConfigHost(key)
		b := Basic{Username: a.Username, Password: a.Password}
		if a.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				return nil, fmt.Errorf("registry config %s: the auth of %s is not base64", path, host)
			}
			user, pass, ok := strings.Cut(string(decoded), ":")
			if !ok {
				return nil, fmt.Errorf("registry config %s: the auth of %s is not username:password", path, host)
			}
			b = Basic{Username: user, Password: pass}
		}
		if b.Username == "" || b.Password == "" {
			return nil, fmt.Errorf("registry config %s: %s has no username and password", path, host)
		}
		out[host] = b
	}
	return out, nil
}

// dockerConfigHost is the host of an auths key: a bare host, or a URL.
func dockerConfigHost(key string) string {
	key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	host, _, _ := strings.Cut(key, "/")
	return host
}
