package directadmin

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/shukiv/gniza/internal/panel"
)

const (
	// DefaultConfPath is DirectAdmin's own configuration.
	DefaultConfPath = "/usr/local/directadmin/conf/directadmin.conf"
	// defaultPanelPort is the port DirectAdmin listens on when its
	// configuration does not say otherwise. A real 1.709 host has no
	// port line at all.
	defaultPanelPort = 2222
)

// PanelURL is where DirectAdmin's own API answers on this server.
//
// It is read from DirectAdmin's configuration rather than assumed. The
// certificate the panel serves carries the name it was told to call
// itself -- `servername` in that file -- and a request to an address
// that merely reaches the same machine fails verification. Nothing here
// skips that verification, because what travels on this request is a
// live session credential.
//
// An empty path reads DirectAdmin's own configuration.
func PanelURL(confPath string) (string, error) {
	if confPath == "" {
		confPath = DefaultConfPath
	}
	body, err := os.ReadFile(confPath)
	if err != nil {
		return "", fmt.Errorf("directadmin: read %s: %w", confPath, err)
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("directadmin: read %s: %w", confPath, err)
	}

	// The name goes into a URL, so it is checked to be a name. A
	// servername carrying a port, a path or a space is a configuration
	// this will not guess at.
	name := values["servername"]
	if err := panel.UsableDomainName(name); err != nil {
		return "", fmt.Errorf("directadmin: %s does not name the panel: %w", confPath, err)
	}

	port := defaultPanelPort
	if given := values["port"]; given != "" {
		port, err = strconv.Atoi(given)
		if err != nil || port < 1 || port > 65535 {
			return "", fmt.Errorf("directadmin: %s gives the panel port as %q", confPath, given)
		}
	}

	// Only ssl=1 is SSL, which is how DirectAdmin reads its own file:
	// the line is absent until somebody turns SSL on, and until then the
	// panel serves plain HTTP. Assuming https for a missing line asked
	// an unencrypted panel a question in a language it does not speak --
	// "http: server gave HTTP response to HTTPS client" -- and the page
	// could not find out who was looking at it.
	//
	// A panel serving plain HTTP is reached over plain HTTP: the session
	// already travels in clear to reach that panel at all, and speaking
	// https to something not listening for it reaches nothing.
	scheme := "http"
	if values["ssl"] == "1" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, name, port), nil
}
