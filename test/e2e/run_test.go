//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"git.wilaris.dev/t-cloud-public-csi-driver/internal/config"
	"git.wilaris.dev/t-cloud-public-csi-driver/internal/evs"
	"git.wilaris.dev/t-cloud-public-csi-driver/test/e2e/settings"
)

const (
	// metadataDocumentURL is read here directly. Confirming which instance
	// the run executes on must not depend on the code the run is about to
	// exercise.
	metadataDocumentURL  = "http://169.254.169.254/openstack/latest/meta_data.json"
	metadataFetchTimeout = 30 * time.Second
	metadataBodyLimit    = 64 << 10
)

// errNoRedirect refuses a redirect away from the fixed link-local metadata endpoint.
var errNoRedirect = errors.New("metadata service returned a redirect")

// environment joins the credentials, which reach a run through the process
// environment only, to the settings resolved from the command line and the
// variables.
type environment struct {
	cloud    evs.Config
	settings *settings.Settings

	// Scenarios read these fields directly.
        // They are copied from settings so call sites do not go through set each time.
	volumeType    string
	driverBinary  string
	pagingVolumes int
}

// loadEnvironment reads the credentials from the process environment, joins
// them to the resolved settings and refuses a run whose project is not
// asserted as approved.
func loadEnvironment(set *settings.Settings, getenv func(string) string) (*environment, error) {
	value := func(name string) string { return strings.TrimSpace(getenv(name)) }

	env := &environment{
		cloud: evs.Config{
			AuthURL:       value(config.EnvAuthURL),
			AccessKey:     value(config.EnvAccessKey),
			SecretKey:     value(config.EnvSecretKey),
			ProjectID:     value(config.EnvProjectID),
			RegionName:    value(config.EnvRegionName),
			SecurityToken: value(config.EnvSecurityToken),
		},
		settings:      set,
		volumeType:    set.VolumeType,
		driverBinary:  set.DriverBinary,
		pagingVolumes: set.PagingVolumes,
	}

	required := map[string]string{
		config.EnvAuthURL:    env.cloud.AuthURL,
		config.EnvAccessKey:  env.cloud.AccessKey,
		config.EnvSecretKey:  env.cloud.SecretKey,
		config.EnvProjectID:  env.cloud.ProjectID,
		config.EnvRegionName: env.cloud.RegionName,
	}
	var missing []string
	for name, given := range required {
		if given == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf(
			"these credentials are read from the process environment only and are absent: %s",
			strings.Join(missing, ", "),
		)
	}

	if set.Approved != env.cloud.ProjectID {
		return nil, fmt.Errorf(
			"the project asserted as approved is not the configured %s, so this run would mutate a"+
				" project nobody approved",
			config.EnvProjectID,
		)
	}

	if info, err := os.Stat(env.driverBinary); err != nil || info.IsDir() {
		return nil, fmt.Errorf("driver binary %q is not an executable file", env.driverBinary)
	}

	return env, nil
}

// secrets maps each populated credential variable to its value, for
// containment checks only.
func (e *environment) secrets() map[string]string {
	found := map[string]string{
		config.EnvAccessKey:     e.cloud.AccessKey,
		config.EnvSecretKey:     e.cloud.SecretKey,
		config.EnvSecurityToken: e.cloud.SecurityToken,
	}
	for name, secret := range found {
		if secret == "" {
			delete(found, name)
		}
	}
	return found
}

// assertContained reports an error naming the credential variable, never
// its value, when a secret appears in text the run produced or was handed.
func (e *environment) assertContained(label string, blobs ...string) error {
	for name, secret := range e.secrets() {
		for _, blob := range blobs {
			if strings.Contains(blob, secret) {
				return fmt.Errorf("%s carries the value of %s", label, name)
			}
		}
	}
	return nil
}

// cloudEnviron renders the credential set as process environment entries.
// Credentials reach a driver process this way and no other: never on a
// command line and never through a file.
func (e *environment) cloudEnviron() []string {
	entries := []string{
		config.EnvAuthURL + "=" + e.cloud.AuthURL,
		config.EnvAccessKey + "=" + e.cloud.AccessKey,
		config.EnvSecretKey + "=" + e.cloud.SecretKey,
		config.EnvProjectID + "=" + e.cloud.ProjectID,
		config.EnvRegionName + "=" + e.cloud.RegionName,
	}
	if e.cloud.SecurityToken != "" {
		entries = append(entries, config.EnvSecurityToken+"="+e.cloud.SecurityToken)
	}
	return entries
}

// instanceIdentity is the part of the metadata document that names the
// running instance.
type instanceIdentity struct {
	ServerID string `json:"uuid"`
	Zone     string `json:"availability_zone"`
}

// fetchInstanceIdentity reads the fixed link-local metadata document,
// ignoring proxy settings and refusing any redirect.
func fetchInstanceIdentity(ctx context.Context) (*instanceIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataDocumentURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build metadata request: %w", err)
	}

	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errNoRedirect
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read metadata document: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata service answered with status %d", resp.StatusCode)
	}

	var identity instanceIdentity
	if err := json.NewDecoder(io.LimitReader(resp.Body, metadataBodyLimit)).
		Decode(&identity); err != nil {
		return nil, fmt.Errorf("decode metadata document: %w", err)
	}

	identity.ServerID = strings.TrimSpace(identity.ServerID)
	identity.Zone = strings.TrimSpace(identity.Zone)
	if identity.ServerID == "" || identity.Zone == "" {
		return nil, fmt.Errorf("metadata document names no server or no availability zone")
	}
	return &identity, nil
}
