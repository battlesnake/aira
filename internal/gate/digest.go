package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// DigestFields follows AIRA's stable sorted NUL-separated digest convention.
func DigestFields(fields ...string) string {
	values := append([]string(nil), fields...)
	sort.Strings(values)
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:])
}

func DigestGate(g GateDefinition) (string, error) {
	if g.Command != nil {
		command := g.Command.Normalized()
		g.Command = &command
	}
	data, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	return DigestFields("gate", string(data)), nil
}
func DigestCanary(c CanaryDeclaration) (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	seed, err := json.Marshal(c.Seed)
	if err != nil {
		return "", err
	}
	return DigestFields("canary", string(data), "seed="+string(seed)), nil
}

// CanonicalPayload creates the ordered logical fields authenticated by the
// durable writer. The key and HMAC operation intentionally live in store.
func CanonicalPayload(kind string, fields map[string]string) []byte {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	values := []string{"type=" + kind}
	for _, k := range keys {
		values = append(values, k+"="+fields[k])
	}
	return []byte(strings.Join(values, "\x00"))
}
