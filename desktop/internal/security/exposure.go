package security

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Exposure is a port anyone who can reach the server may connect to.
type Exposure struct {
	Port      int      `json:"port"`
	PortEnd   int      `json:"portEnd,omitempty"` // last port of a published range
	Proto     string   `json:"proto"`
	Addrs     []string `json:"addrs"`               // addresses it listens on (0.0.0.0, ::, or one address)
	Container string   `json:"container,omitempty"` // "" for a program on the server itself
	Target    string   `json:"target,omitempty"`    // the port inside the container (5432/tcp)
	Service   string   `json:"service,omitempty"`   // what usually runs there (PostgreSQL, Redis…)
	Risk      string   `json:"risk"`                // critical, warning, info
	Firewall  string   `json:"firewall"`            // what the firewall does with it, in plain words
	Fix       string   `json:"fix,omitempty"`
}

type service struct {
	name string
	tier int // tierSensitive: data or admin access; tierCommon: meant to be public
}

const (
	tierOther = iota
	tierCommon
	tierSensitive
)

var wellKnown = map[int]service{
	21: {"FTP", tierSensitive}, 22: {"SSH", tierCommon}, 23: {"Telnet", tierSensitive}, 25: {"SMTP", tierCommon},
	53: {"DNS", tierCommon}, 80: {"HTTP", tierCommon}, 443: {"HTTPS", tierCommon}, 465: {"SMTPS", tierCommon},
	587: {"SMTP submission", tierCommon}, 993: {"IMAPS", tierCommon}, 995: {"POP3S", tierCommon},
	111: {"rpcbind", tierSensitive}, 445: {"SMB", tierSensitive}, 1433: {"SQL Server", tierSensitive},
	1521: {"Oracle", tierSensitive}, 2181: {"ZooKeeper", tierSensitive}, 2375: {"Docker API without TLS", tierSensitive},
	2376: {"Docker API", tierSensitive}, 2379: {"etcd", tierSensitive}, 2380: {"etcd", tierSensitive},
	3306: {"MySQL / MariaDB", tierSensitive}, 3389: {"Remote Desktop", tierSensitive}, 4369: {"Erlang port mapper", tierSensitive},
	5432: {"PostgreSQL", tierSensitive}, 5601: {"Kibana", tierSensitive}, 5672: {"RabbitMQ", tierSensitive},
	5900: {"VNC", tierSensitive}, 5984: {"CouchDB", tierSensitive}, 6379: {"Redis", tierSensitive},
	8086: {"InfluxDB", tierSensitive}, 8500: {"Consul", tierSensitive}, 9042: {"Cassandra", tierSensitive},
	9092: {"Kafka", tierSensitive}, 9200: {"Elasticsearch", tierSensitive}, 9300: {"Elasticsearch", tierSensitive},
	10250: {"Kubelet", tierSensitive}, 11211: {"Memcached", tierSensitive}, 15672: {"RabbitMQ admin", tierSensitive},
	27017: {"MongoDB", tierSensitive}, 28017: {"MongoDB", tierSensitive},
}

// serviceFor names a port; for a container the port inside it says more (5432 published as 15432).
func serviceFor(ports ...int) service {
	for _, p := range ports {
		if s, ok := wellKnown[p]; ok {
			return s
		}
	}
	return service{}
}

func isLoopback(addr string) bool {
	a := strings.Trim(addr, "[]")
	return strings.HasPrefix(a, "127.") || a == "::1" || a == "localhost" || strings.HasPrefix(a, "::ffff:127.")
}

func normAddr(a string) string {
	a = strings.Trim(a, "[]")
	if a == "" || a == "*" {
		return "0.0.0.0"
	}
	if i := strings.Index(a, "%"); i >= 0 {
		a = a[:i]
	}
	return a
}

var dockerPortRe = regexp.MustCompile(`^(.*):(\d+)(?:-(\d+))?->(\d+)(?:-(\d+))?/(tcp|udp|sctp)$`)

func exposures(fw Firewall, dockerLines, listenLines []string, sshPorts []int, root bool) []Exposure {
	out := []Exposure{}
	published := map[string]bool{} // proto/port published by Docker (docker-proxy listens there too)
	type key struct {
		c         string
		port, end int
		proto     string
	}
	idx := map[key]int{}
	for _, l := range dockerLines {
		name, ports, ok := strings.Cut(l, "\t")
		if !ok {
			continue
		}
		for _, part := range strings.Split(ports, ",") {
			m := dockerPortRe.FindStringSubmatch(strings.TrimSpace(part))
			if m == nil {
				continue
			}
			lo, _ := strconv.Atoi(m[2])
			hi := lo
			if m[3] != "" {
				hi, _ = strconv.Atoi(m[3])
			}
			in, _ := strconv.Atoi(m[4])
			proto := m[6]
			for p := lo; p <= hi && p-lo < 2000; p++ {
				published[proto+"/"+strconv.Itoa(p)] = true
			}
			if isLoopback(m[1]) {
				continue
			}
			addr := normAddr(m[1])
			k := key{name, lo, hi, proto}
			if i, ok := idx[k]; ok {
				if !contains(out[i].Addrs, addr) {
					out[i].Addrs = append(out[i].Addrs, addr)
				}
				continue
			}
			e := Exposure{Port: lo, Proto: proto, Addrs: []string{addr}, Container: name, Target: m[4] + "/" + proto}
			if hi != lo {
				e.PortEnd = hi
				e.Target = m[4] + "-" + orStr(m[5], m[4]) + "/" + proto
			}
			e.Service = serviceFor(in, lo).name
			classify(&e, fw, root, serviceFor(in, lo).tier, true)
			idx[k] = len(out)
			out = append(out, e)
		}
	}

	// Programs on the server itself (TCP).
	hostIdx := map[int]int{}
	for _, l := range listenLines {
		f := strings.Fields(l)
		if len(f) < 4 || f[0] != "LISTEN" {
			continue
		}
		local := f[3]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(local[i+1:])
		if err != nil || published["tcp/"+strconv.Itoa(port)] || isLoopback(local[:i]) {
			continue
		}
		addr := normAddr(local[:i])
		if j, ok := hostIdx[port]; ok {
			if j >= 0 && !contains(out[j].Addrs, addr) {
				out[j].Addrs = append(out[j].Addrs, addr)
			}
			continue
		}
		s := serviceFor(port)
		if containsInt(sshPorts, port) {
			s = service{"SSH", tierCommon}
		}
		e := Exposure{Port: port, Proto: "tcp", Addrs: []string{addr}, Service: s.name}
		if !classify(&e, fw, root, s.tier, false) {
			hostIdx[port] = -1 // blocked by the firewall
			continue
		}
		hostIdx[port] = len(out)
		out = append(out, e)
	}

	rank := map[string]int{"critical": 0, "warning": 1, "info": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Risk] != rank[out[j].Risk] {
			return rank[out[i].Risk] < rank[out[j].Risk]
		}
		return out[i].Port < out[j].Port
	})
	return out
}

// classify sets the risk, what the firewall does and the fix. It returns false for a program on the server
// that the firewall blocks (not exposed).
func classify(e *Exposure, fw Firewall, root bool, tier int, docker bool) bool {
	pick := func(sensitive, common, other string) string {
		switch tier {
		case tierSensitive:
			return sensitive
		case tierCommon:
			return common
		}
		return other
	}
	what := "it"
	if e.Service != "" {
		what = e.Service
	}
	localOnly := ""
	if docker {
		inner := strings.SplitN(e.Target, "/", 2)[0]
		localOnly = fmt.Sprintf("Publish it on 127.0.0.1 only (-p 127.0.0.1:%d:%s) if only this server uses it", e.Port, inner)
		if e.PortEnd > 0 {
			localOnly = "Publish it on 127.0.0.1 only (-p 127.0.0.1:…) if only this server uses it"
		}
	}
	switch {
	case !fw.Readable:
		e.Firewall = "Unknown: reading the firewall needs root"
		e.Risk = pick("warning", "info", "info")
		if tier == tierSensitive {
			e.Fix = orStr(localOnly, "Make sure a firewall allows "+what+" only from the addresses that need it") + "."
		}
		return true

	case docker && fw.DockerUser:
		e.Firewall = "Filtered by your DOCKER-USER rules (check that they cover this port)"
		e.Risk = pick("warning", "info", "info")
		return true

	case fw.Kind == "none":
		e.Firewall = "No firewall: open to anyone who can reach the server"
		e.Risk = pick("critical", "info", "warning")
		if tier != tierCommon {
			if docker {
				e.Fix = localOnly + ", or turn on the firewall and filter it in the DOCKER-USER chain."
			} else {
				e.Fix = "Turn on the firewall and allow only the ports you need, or make " + what + " listen on 127.0.0.1."
			}
		}
		return true

	case docker && fw.Kind == "ufw":
		if Verdict(fw.Rules, fw.DefaultIn, e.Port, e.Proto, nil) == Allowed {
			e.Firewall = "Allowed by ufw"
			e.Risk = pick("warning", "info", "info")
			if tier == tierSensitive {
				e.Fix = localOnly + ", or allow it only from the addresses that need it (DOCKER-USER chain)."
			}
			return true
		}
		e.Firewall = "Not allowed by ufw, but open anyway: Docker's rules come before ufw's"
		e.Risk = pick("critical", "warning", "warning")
		e.Fix = localOnly + ". If it must be public, allow it in ufw so the rules say so; to filter it, add rules to the DOCKER-USER chain."
		return true

	case docker:
		e.Firewall = "Open: Docker's rules come before your firewall's INPUT rules"
		e.Risk = pick("critical", "warning", "warning")
		e.Fix = localOnly + ", or filter it in the DOCKER-USER chain."
		return true

	case fw.Kind == "ufw":
		switch Verdict(fw.Rules, fw.DefaultIn, e.Port, e.Proto, nil) {
		case Allowed:
			e.Firewall = "Allowed by ufw from anywhere"
			e.Risk = pick("warning", "info", "info")
			if tier == tierSensitive {
				e.Fix = "Allow " + what + " only from the addresses that need it."
			}
			return true
		case Some:
			e.Firewall = "Allowed by ufw from some addresses only"
			e.Risk = "info"
			return true
		}
		return false
	}
	return false // another firewall: its rules are not read here
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func containsInt(list []int, n int) bool {
	for _, x := range list {
		if x == n {
			return true
		}
	}
	return false
}
