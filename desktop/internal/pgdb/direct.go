package pgdb

// Direct transfer: when the target server can reach the source database over the network, pg_dump runs in
// the target container and reads the source over N parallel connections, so nothing goes through this computer:
//
//	server A: PostgreSQL  ←── N connections ───  server B: pg_dump -Fd -j N  →  pg_restore -j N
//
// The source's login comes from its container (as everywhere else) and reaches the target container through
// SSH stdin into a pgpass file, never on a command line.
//
// Detection tries the source server's own addresses (private ones first, then the address the app connects
// to) with the port that publishes the container's 5432, and only accepts a server whose identity matches
// the source: an address like 172.17.0.1 could otherwise lead the target to its own PostgreSQL.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var hostRe = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,253}$`)

// Address is where the target reaches the source database.
type Address struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (a Address) String() string { return net.JoinHostPort(a.Host, strconv.Itoa(a.Port)) }

// DirectCheck says whether the target can read the source directly.
type DirectCheck struct {
	OK      bool     `json:"ok"`
	Address *Address `json:"address,omitempty"`
	Message string   `json:"message"`
}

// login is how the target logs in to the source, and what the source looks like from inside.
type login struct {
	user, password, identity string
}

// identitySQL tells one PostgreSQL server (and database) from another; any user may run it.
const identitySQL = `select pg_postmaster_start_time()::text || '/' || oid from pg_database where datname = current_database()`

// publishedPort returns the host port that publishes the container's 5432 and the address it is bound to.
func publishedPort(ctx context.Context, s Side, container string) (port int, bind string, err error) {
	out, err := run(ctx, s.Host, s.Docker+" port "+container+" 5432/tcp", 30*time.Second)
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			i := strings.LastIndex(line, ":")
			if i < 0 {
				continue
			}
			if p, err := strconv.Atoi(line[i+1:]); err == nil && p > 0 {
				return p, strings.Trim(line[:i], "[]"), nil
			}
		}
	}
	return 0, "", fmt.Errorf("port 5432 of %s is not published on %s (docker run -p 5432:5432, or ports: in compose), so other servers cannot reach it", container, s.Server)
}

// Interfaces of container networks, bridges and overlays: their addresses mean nothing to another server.
var virtualIface = regexp.MustCompile(`^(docker|br-|veth|cni|flannel|cali|virbr|kube|cilium|vxlan|lxc|lxd|podman|weave|tunl0|lo$)`)

// serverAddresses lists the IPv4 addresses of the source server's real interfaces, private ones first.
func serverAddresses(ctx context.Context, s Side) []string {
	out, err := run(ctx, s.Host, "ip -o -4 addr show scope global 2>/dev/null", 20*time.Second)
	if err != nil {
		return nil
	}
	var private, public []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[2] != "inet" || virtualIface.MatchString(f[1]) {
			continue
		}
		ip, _, err := net.ParseCIDR(f[3])
		if err != nil {
			continue
		}
		if ip.IsPrivate() {
			private = append(private, ip.String())
		} else {
			public = append(public, ip.String())
		}
	}
	return append(private, public...)
}

// candidates are the addresses to try, best first.
func candidates(ctx context.Context, spec Spec, src *Side) ([]Address, error) {
	port := spec.DirectPort
	bind := ""
	if port == 0 {
		p, b, err := publishedPort(ctx, *src, spec.Source.Container)
		if err != nil {
			return nil, err
		}
		port, bind = p, b
	}
	if spec.DirectHost != "" {
		return []Address{{spec.DirectHost, port}}, nil
	}
	if strings.HasPrefix(bind, "127.") || bind == "::1" {
		return nil, fmt.Errorf("port %d of %s is published on %s only, so other servers cannot reach it", port, spec.Source.Container, bind)
	}
	var hosts []string
	if bind != "" && bind != "0.0.0.0" && bind != "::" {
		hosts = []string{bind} // published on one address only
	} else {
		hosts = append(serverAddresses(ctx, *src), src.Address)
	}
	var list []Address
	seen := map[string]bool{}
	for _, h := range hosts {
		if h != "" && !seen[h] && hostRe.MatchString(h) && len(list) < 5 {
			seen[h] = true
			list = append(list, Address{h, port})
		}
	}
	if len(list) == 0 {
		return nil, errors.New("enter the address of the source server as the target sees it")
	}
	return list, nil
}

// sourceLogin reads the user and password the source container logs in with, and the source's identity.
func sourceLogin(ctx context.Context, src Side, container, db string) (login, error) {
	script := pgEnv + `id=$(psql -XAtq -d ` + quote(db) + ` -c "` + identitySQL + `" 2>/dev/null)
printf '%s\n%s\n%s' "$id" "$PGUSER" "$PGPASSWORD"`
	out, err := run(ctx, src.Host, dexec(src.Docker, container, false, script), 30*time.Second)
	if err != nil {
		return login{}, fmt.Errorf("could not read the source login: %w", err)
	}
	parts := strings.SplitN(out, "\n", 3)
	if len(parts) < 3 || parts[0] == "" {
		return login{}, errors.New("could not query the source database")
	}
	return login{identity: parts[0], user: parts[1], password: parts[2]}, nil
}

// pgpass is one line of a .pgpass file for any host, port and database.
func pgpass(user, password string) string {
	esc := strings.NewReplacer(`\`, `\\`, `:`, `\:`)
	return "*:*:*:" + esc.Replace(user) + ":" + esc.Replace(password) + "\n"
}

// directEnv points the PostgreSQL tools of the target container at the source. The target's own
// PGPASSWORD (from pgEnv) is removed, or it would win over the pgpass file.
func directEnv(a Address, user, passfile string, timeout int) string {
	return fmt.Sprintf("env -u PGPASSWORD PGHOST=%s PGPORT=%d PGUSER=%s PGPASSFILE=%s PGCONNECT_TIMEOUT=%d ", quote(a.Host), a.Port, quote(user), passfile, timeout)
}

// writePgpass stores the source's login in the target container (readable by its owner only).
func writePgpass(ctx context.Context, dst Side, container, path string, l login) error {
	err := dst.Host.Run(ctx, dexec(dst.Docker, container, true, "umask 077; cat > "+path), strings.NewReader(pgpass(l.user, l.password)), nil, nil)
	if err != nil {
		return fmt.Errorf("could not store the source login on the target: %w", err)
	}
	return nil
}

// tryDirect logs in to the source database from the target container at one address.
func tryDirect(ctx context.Context, dst Side, spec Spec, a Address, l login, timeout int) error {
	// The pgpass file only lives for this one command.
	script := "umask 077; f=/tmp/sd-check-$$; cat > $f\n" + pgEnv +
		directEnv(a, l.user, "$f", timeout) + "psql -XAtq -d " + quote(spec.Source.Database) + " -c \"" + identitySQL + "\" 2>&1\nrc=$?; rm -f $f; exit $rc"
	var out strings.Builder
	err := dst.Host.Run(ctx, dexec(dst.Docker, spec.Target.Container, true, script), strings.NewReader(pgpass(l.user, l.password)), &out, &out)
	msg := strings.TrimSpace(out.String())
	if err == nil {
		if msg != l.identity {
			return fmt.Errorf("%s is another PostgreSQL server, not the source", a)
		}
		return nil
	}
	switch {
	case strings.Contains(msg, "timeout expired") || strings.Contains(msg, "timed out"):
		return fmt.Errorf("cannot reach %s (timed out: a firewall, or another network)", a)
	case strings.Contains(msg, "Connection refused"):
		return fmt.Errorf("%s refuses the connection (nothing listens there)", a)
	case strings.Contains(msg, "No route to host") || strings.Contains(msg, "Network is unreachable"):
		return fmt.Errorf("no route to %s", a)
	case strings.Contains(msg, "no pg_hba.conf entry"):
		return fmt.Errorf("the source at %s refuses the target: add the target's address to pg_hba.conf on the source", a)
	case strings.Contains(msg, "could not translate host name"):
		return fmt.Errorf("the target cannot resolve %s", a.Host)
	case strings.Contains(msg, "password authentication failed"):
		return fmt.Errorf("%s answered but refused the source's login (another server?)", a)
	case msg != "":
		return fmt.Errorf("%s: %s", a, lastLines(msg, 2))
	}
	return err
}

// detectDirect finds an address at which the target reaches the source. timeout is the connect timeout
// of each try, in seconds.
func detectDirect(ctx context.Context, spec Spec, src, dst *Side, srcMajor, dstMajor, timeout int) (DirectCheck, login) {
	fail := func(format string, a ...any) (DirectCheck, login) {
		return DirectCheck{Message: fmt.Sprintf(format, a...)}, login{}
	}
	if dstMajor > 0 && srcMajor > 0 && dstMajor < srcMajor {
		return fail("the target runs an older PostgreSQL (%d) than the source (%d), and a direct transfer runs pg_dump on the target", dstMajor, srcMajor)
	}
	list, err := candidates(ctx, spec, src)
	if err != nil {
		return fail("%s", err)
	}
	l, err := sourceLogin(ctx, *src, spec.Source.Container, spec.Source.Database)
	if err != nil {
		return fail("%s", err)
	}
	var errs []string
	for _, a := range list {
		if ctx.Err() != nil {
			return fail("%s", ctx.Err())
		}
		if err := tryDirect(ctx, *dst, spec, a, l, timeout); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		return DirectCheck{OK: true, Address: &Address{a.Host, a.Port},
			Message: fmt.Sprintf("%s reaches the source database at %s", dst.Server, a)}, l
	}
	return fail("%s", strings.Join(errs, "; "))
}

// DetectDirect checks, before a copy, whether the target can read the source directly.
func DetectDirect(ctx context.Context, spec Spec, src, dst *Side) DirectCheck {
	spec.Kind, spec.Direct, spec.File = "copy", true, ""
	if err := spec.Validate(); err != nil {
		return DirectCheck{Message: capital(err.Error())}
	}
	if sameContainer(spec) {
		return DirectCheck{Message: "the source and the target are the same container"}
	}
	var srcIn, dstIn Instance
	var srcErr, dstErr error
	done := make(chan struct{})
	go func() { srcIn, srcErr = Probe(ctx, *src, spec.Source.Container); close(done) }()
	dstIn, dstErr = Probe(ctx, *dst, spec.Target.Container)
	<-done
	if srcErr != nil {
		return DirectCheck{Message: "source: " + srcErr.Error()}
	}
	if dstErr != nil {
		return DirectCheck{Message: "target: " + dstErr.Error()}
	}
	c, _ := detectDirect(ctx, spec, src, dst, srcIn.Major(), dstIn.Major(), 4)
	return c
}
