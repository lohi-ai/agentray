package credential

import "strings"

// EnvPrefix is the host-env namespace a credential is loaded from: an operator
// sets AGENTRAY_CRED_NOVEL_API_KEY=sk-… and the agent references it as
// {{cred:NOVEL_API_KEY}}. The prefix keeps vault secrets distinct from ordinary
// process configuration.
const EnvPrefix = "AGENTRAY_CRED_"

// LoadFromEnviron builds a Vault from the entries of environ (the os.Environ()
// "KEY=value" form) whose key carries EnvPrefix. The credential name is the key
// with the prefix stripped, so AGENTRAY_CRED_NOVEL_API_KEY becomes NOVEL_API_KEY.
//
// It is pure (takes the environ slice rather than reading os.Environ itself) so
// it is unit-testable, and skips malformed or empty entries rather than failing
// — a single bad var must not stop the host from booting.
func LoadFromEnviron(environ []string) *Vault {
	v := NewVault()
	for _, kv := range environ {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key, value := kv[:eq], kv[eq+1:]
		if !strings.HasPrefix(key, EnvPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, EnvPrefix)
		// Put validates the name and rejects empties; ignore the error so one
		// malformed credential is skipped, not fatal.
		_ = v.Put(name, value)
	}
	return v
}
