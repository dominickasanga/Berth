package deploy

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// parseComposeVersion reads "2.39.1", "v2.29.7" or "1.29.2" into major and minor.
func parseComposeVersion(out string) (major, minor int, ok bool) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, true
}

// CheckCompose runs "<compose> version --short". Compose v1 is refused (Berth needs
// `up --wait`, several --env-file flags and `--profile "*"`); v2 older than 2.24 only warns.
func CheckCompose(cmd []string) (version, warning string, err error) {
	if len(cmd) == 0 {
		cmd = []string{"docker", "compose"}
	}
	out, runErr := exec.Command(cmd[0], append(append([]string{}, cmd[1:]...), "version", "--short")...).CombinedOutput()
	if runErr != nil {
		return "", "", fmt.Errorf("cannot run %q: %v. Berth needs Docker Compose v2: install the docker-compose-plugin (so `docker compose version` works) or set BERTH_COMPOSE_CMD", strings.Join(cmd, " "), strings.Join(strings.Fields(string(out)), " "))
	}
	major, minor, ok := parseComposeVersion(string(out))
	if !ok {
		return "", "", fmt.Errorf("could not read the version from %q", strings.TrimSpace(string(out)))
	}
	version = fmt.Sprintf("%d.%d", major, minor)
	if major < 2 {
		return version, "", fmt.Errorf("Docker Compose %s found, but Berth needs Compose v2 (2.24 or newer). Install the docker-compose-plugin; v1 can stay installed beside it", version)
	}
	if major == 2 && minor < 24 {
		warning = fmt.Sprintf("Docker Compose %s is older than 2.24; some apps (several --env-file flags, env_file required:false) may fail", version)
	}
	return version, warning, nil
}
