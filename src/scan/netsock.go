package scan

import (
	"strconv"
	"strings"
)

// ztunnel's in-pod listener ports. A captured ambient pod has these LISTENing
// inside its own network namespace; an orphan that lost capture does not.
//
//	15001 — outbound capture
//	15006 — inbound plaintext
//	15008 — inbound HBONE (mTLS)
//	15053 — DNS capture
var ztunnelInPodPorts = []int{15001, 15006, 15008, 15053}

// captureRequiredPorts are the listeners whose presence we treat as "captured".
// 15008 (inbound HBONE) and 15001 (outbound) are the definitive ambient-capture
// indicators; their absence on an ambient-annotated pod is the orphan signature.
var captureRequiredPorts = []int{15001, 15008}

// tcpStateListen is the /proc/net/tcp `st` value for TCP_LISTEN.
const tcpStateListen = "0A"

// parseListenPorts parses the contents of /proc/net/tcp and /proc/net/tcp6 and
// returns which of the wanted ports are in LISTEN state. The local_address
// column is "HEXIP:HEXPORT"; for tcp6 the IP is a 32-char hex string with no
// embedded colons, so the last ':' always precedes the port.
func parseListenPorts(procNet string, wanted []int) []int {
	want := make(map[int]bool, len(wanted))
	for _, w := range wanted {
		want[w] = true
	}
	found := make(map[int]bool)
	for _, line := range strings.Split(procNet, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != tcpStateListen {
			continue
		}
		i := strings.LastIndex(f[1], ":")
		if i < 0 {
			continue
		}
		port, err := strconv.ParseInt(f[1][i+1:], 16, 32)
		if err != nil {
			continue
		}
		if want[int(port)] {
			found[int(port)] = true
		}
	}
	out := make([]int, 0, len(found))
	for _, w := range wanted { // stable, wanted order
		if found[w] {
			out = append(out, w)
		}
	}
	return out
}

// isCaptured reports whether the present ports include all of the required
// ambient-capture listeners.
func isCaptured(present []int) bool {
	set := make(map[int]bool, len(present))
	for _, p := range present {
		set[p] = true
	}
	for _, r := range captureRequiredPorts {
		if !set[r] {
			return false
		}
	}
	return true
}
