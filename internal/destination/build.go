package destination

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Spec is a destination's stored configuration plus the secrets resolved
// for one job. The controller keeps Config in the database and Secrets in
// the vault; they are only combined when a job is dispatched.
type Spec struct {
	Type Type `json:"type"`
	// Config holds non-sensitive settings: hosts, buckets, paths, ports.
	Config map[string]string `json:"config"`
	// Secrets holds credentials. They are resolved per job, delivered over
	// mTLS, and never persisted by the agent.
	Secrets map[string]string `json:"secrets,omitempty"`
}

// Build turns a Spec into a Destination.
//
// Unknown configuration keys are an error rather than an ignored typo: a
// silently dropped "endpoint" would send backups to the wrong provider.
func Build(spec Spec) (Destination, error) {
	reader := &specReader{spec: spec}

	var dest Destination
	switch spec.Type {
	case TypeLocal:
		dest = &Local{Root: reader.config("root", true)}
	case TypeSFTP:
		// The login is a key unless the configuration says password, in
		// which case the password is a secret and the askpass program a
		// path, and a key is not needed.
		byPassword := reader.config("auth", false) == "password"
		sftp := &SFTP{
			Host:           reader.config("host", true),
			Port:           reader.intConfig("port"),
			User:           reader.config("user", true),
			Root:           reader.config("root", true),
			IdentityFile:   reader.config("identity_file", !byPassword),
			KnownHostsFile: reader.config("known_hosts_file", true),
		}
		askPass := reader.config("askpass_file", byPassword)
		if byPassword {
			sftp.Password = reader.secret("password", true)
			sftp.AskPass = askPass
		}
		dest = sftp
	case TypeREST:
		// Accepted here so the unknown-key check does not reject it; it is
		// applied by ForMaintenance, not used directly.
		reader.markUsed(maintenanceBaseURLKey)
		dest = &REST{
			BaseURL:    reader.config("base_url", true),
			AppendOnly: reader.boolConfig("append_only"),
			CABundle:   reader.config("ca_bundle", false),
			Username:   reader.secret("username", true),
			Password:   reader.secret("password", false),
		}
	case TypeS3:
		dest = &S3{
			Endpoint:        reader.config("endpoint", false),
			Region:          reader.config("region", false),
			Bucket:          reader.config("bucket", true),
			AccessKeyID:     reader.secret("access_key_id", true),
			SecretAccessKey: reader.secret("secret_access_key", true),
			SessionToken:    reader.secret("session_token", false),
		}
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedType, spec.Type)
	}

	if err := reader.err(); err != nil {
		return nil, err
	}
	return dest, nil
}

// maintenanceBaseURLKey names a second endpoint for the same storage that
// permits deletes.
//
// rest-server's --append-only is a property of the running process, not of
// a credential: with it enabled, nothing can delete through that endpoint,
// including the maintenance runner. The standard deployment is therefore
// two rest-server instances over one data directory — an append-only one
// the agents reach, and a delete-capable one bound to a management network
// that only the maintenance runner reaches. See docs/DESIGN.md §8.
const maintenanceBaseURLKey = "maintenance_base_url"

// ForMaintenance returns a copy of spec addressed to the endpoint that
// permits deletes, where the destination has one.
//
// Without an override the spec is returned unchanged: a local, SFTP or S3
// destination is delete-capable already, and an append-only rest-server
// with no second endpoint simply cannot be pruned — which the caller finds
// out as an error rather than as silent unbounded growth.
func ForMaintenance(spec Spec) Spec {
	override, present := spec.Config[maintenanceBaseURLKey]
	if !present || override == "" {
		return spec
	}

	config := make(map[string]string, len(spec.Config))
	for key, value := range spec.Config {
		config[key] = value
	}
	config["base_url"] = override
	// The maintenance endpoint is by definition the one that is not
	// append-only.
	delete(config, "append_only")

	return Spec{Type: spec.Type, Config: config, Secrets: spec.Secrets}
}

// ParseSpec decodes a stored JSON configuration into a Spec's Config map.
func ParseSpec(destType Type, configJSON []byte, secrets map[string]string) (Spec, error) {
	config := map[string]string{}
	if len(configJSON) > 0 {
		raw := map[string]any{}
		if err := json.Unmarshal(configJSON, &raw); err != nil {
			return Spec{}, fmt.Errorf("destination: parse config: %w", err)
		}
		for key, value := range raw {
			switch typed := value.(type) {
			case string:
				config[key] = typed
			case bool:
				config[key] = strconv.FormatBool(typed)
			case float64:
				// JSON numbers arrive as float64; ports and sizes are integers.
				config[key] = strconv.FormatInt(int64(typed), 10)
			default:
				return Spec{}, fmt.Errorf("destination: config key %q has unsupported type %T", key, value)
			}
		}
	}
	return Spec{Type: destType, Config: config, Secrets: secrets}, nil
}

// specReader pulls typed values out of a Spec, collecting problems so that
// building a badly configured destination reports every missing field at
// once instead of one per attempt.
type specReader struct {
	spec     Spec
	problems []string
	used     map[string]bool
}

func (r *specReader) config(key string, required bool) string {
	r.markUsed(key)
	value, present := r.spec.Config[key]
	if required && value == "" {
		if present {
			r.problems = append(r.problems, fmt.Sprintf("config %q is empty", key))
		} else {
			r.problems = append(r.problems, fmt.Sprintf("config %q is required", key))
		}
	}
	return value
}

func (r *specReader) secret(key string, required bool) string {
	value := r.spec.Secrets[key]
	if required && value == "" {
		r.problems = append(r.problems, fmt.Sprintf("secret %q is required", key))
	}
	return value
}

func (r *specReader) intConfig(key string) int {
	r.markUsed(key)
	raw := r.spec.Config[key]
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		r.problems = append(r.problems, fmt.Sprintf("config %q must be a number, got %q", key, raw))
		return 0
	}
	return value
}

func (r *specReader) boolConfig(key string) bool {
	r.markUsed(key)
	raw := r.spec.Config[key]
	if raw == "" {
		return false
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		r.problems = append(r.problems, fmt.Sprintf("config %q must be a boolean, got %q", key, raw))
		return false
	}
	return value
}

func (r *specReader) markUsed(key string) {
	if r.used == nil {
		r.used = map[string]bool{}
	}
	r.used[key] = true
}

func (r *specReader) err() error {
	for key := range r.spec.Config {
		if !r.used[key] {
			r.problems = append(r.problems,
				fmt.Sprintf("config %q is not used by a %s destination", key, r.spec.Type))
		}
	}
	if len(r.problems) == 0 {
		return nil
	}
	return fmt.Errorf("destination %s: %s", r.spec.Type, joinProblems(r.problems))
}

func joinProblems(problems []string) string {
	out := problems[0]
	for _, problem := range problems[1:] {
		out += "; " + problem
	}
	return out
}
