package remote

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// On Linux, Bonjour is Avahi: avahi-publish-service advertises this host and
// avahi-browse finds the others. Both come with the avahi-utils package; a
// computer without them pairs by address only.

func advertiseAvahi(name string, port int, fp string) func() {
	publish, err := exec.LookPath("avahi-publish-service")
	if err != nil {
		return func() {}
	}
	cmd := exec.Command(publish, "-s", name, serviceType, strconv.Itoa(port), "v=1", "fp="+fp[:16])
	if err := cmd.Start(); err != nil {
		return func() {}
	}
	go cmd.Wait()
	return func() { cmd.Process.Kill() }
}

func discoverAvahi(ctx context.Context, selfFP string) []Found {
	browse, err := exec.LookPath("avahi-browse")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// --terminate stops once the cache is complete; --resolve adds the
	// address, port and TXT record; --parsable makes it one line each.
	out, _ := exec.CommandContext(ctx, browse, "--resolve", "--parsable", "--terminate", serviceType).Output()
	return parseAvahi(out, selfFP)
}

// parseAvahi reads avahi-browse's resolved lines:
//
//	=;eth0;IPv4;Studio\032Mac;_switcher._tcp;local;studio.local;192.168.1.5;8788;"v=1" "fp=..."
func parseAvahi(out []byte, selfFP string) []Found {
	byName := map[string]Found{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), ";", 10)
		if len(fields) < 10 || fields[0] != "=" || fields[4] != serviceType {
			continue
		}
		port, err := strconv.Atoi(fields[8])
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		f := Found{Name: avahiUnescape(fields[3]), Address: fields[6], Port: port}
		if m := fpField.FindStringSubmatch(fields[9]); m != nil {
			f.fp = m[1]
		}
		f.Self = selfFP != "" && f.fp != "" && strings.HasPrefix(selfFP, f.fp)
		// One entry per host: IPv4 over IPv6, as the Mac's dns-sd lists it.
		if previous, seen := byName[f.Name]; seen && previous.Port != 0 && fields[2] != "IPv4" {
			continue
		}
		if f.Address == "" {
			f.Address = fields[7]
		}
		byName[f.Name] = f
	}
	found := make([]Found, 0, len(byName))
	for _, f := range byName {
		found = append(found, f)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found
}

// avahiUnescape decodes avahi-browse's \DDD (decimal byte) and \c escapes.
func avahiUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		if i+3 < len(s) {
			if n, err := strconv.Atoi(s[i+1 : i+4]); err == nil && n < 256 {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i+1])
		i++
	}
	return b.String()
}

// avahiRunning reports whether this computer answers .local names.
func avahiRunning() bool {
	for _, socket := range []string{"/run/avahi-daemon/socket", "/var/run/avahi-daemon/socket"} {
		if _, err := os.Stat(socket); err == nil {
			return true
		}
	}
	return false
}

// prettyHostname is the name people gave this computer, from
// /etc/machine-info, as hostnamectl shows it.
func prettyHostname(machineInfo []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(machineInfo))
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !ok || key != "PRETTY_HOSTNAME" {
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			return strings.TrimSpace(unquoted)
		}
		return strings.Trim(strings.TrimSpace(value), `'"`)
	}
	return ""
}
