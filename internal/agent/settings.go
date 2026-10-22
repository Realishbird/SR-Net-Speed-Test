package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// Host-side RoCE settings (Phase 5). The fabric check on the controller
// compares these with the fabric profile and with the switch port on the
// other end of the cable. Everything here is read-only and works without
// root: the mlx5 driver exposes ECN and RoCE mode under sysfs, and the
// iproute2 `dcb` tool (or Mellanox's mlnx_qos) reads PFC, trust and the
// DSCP map over DCB netlink.

// addSettings fills Settings on every port that has a netdev.
func addSettings(ctx context.Context, ports []api.Port) {
	for i := range ports {
		if ports[i].Netdev == "" {
			continue
		}
		s := readSettings(ctx, ports[i].RdmaDev, ports[i].PortNum, ports[i].Netdev)
		ports[i].Settings = &s
	}
}

func readSettings(ctx context.Context, dev string, port int, netdev string) api.PortSettings {
	s := api.PortSettings{CNPDSCP: -1, CNPPrio: -1, TOS: -1}
	var src []string
	s.RoCEMode = roceMode(dev, port)
	if np, rp, cnpDSCP, cnpPrio, ok := sysfsECN(netdev); ok {
		s.ECNNp, s.ECNRp, s.CNPDSCP, s.CNPPrio = np, rp, cnpDSCP, cnpPrio
		src = append(src, "sysfs")
	}
	if tc := strings.TrimSpace(readFile(filepath.Join(sysRoot, "class/infiniband", dev, "tc", strconv.Itoa(port), "traffic_class"))); tc != "" {
		if n, err := strconv.Atoi(tc); err == nil {
			s.TOS = n
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "dcb", "-j", "pfc", "show", "dev", netdev).Output(); err == nil {
		if pfc, ok := ParseDCBPFC(string(out)); ok {
			s.PFC = pfc
			src = append(src, "dcb")
		}
		if out, err := exec.CommandContext(ctx, "dcb", "-j", "app", "show", "dev", netdev).Output(); err == nil {
			s.DSCPPrio, s.Trust = ParseDCBApp(string(out))
		}
	}
	if s.PFC == "" {
		if out, err := exec.CommandContext(ctx, "mlnx_qos", "-i", netdev).Output(); err == nil {
			m := ParseMlnxQos(string(out))
			if m.PFC != "" {
				s.PFC, src = m.PFC, append(src, "mlnx_qos")
			}
			if s.Trust == "" {
				s.Trust = m.Trust
			}
			if s.DSCPPrio == "" {
				s.DSCPPrio = m.DSCPPrio
			}
		}
	}
	s.Source = strings.Join(src, ",")
	return s
}

// roceMode reports v2 when the port offers a RoCE v2 GID, v1 when only v1
// GIDs exist, and "" when the driver exposes no GID types (InfiniBand, or
// an older kernel).
func roceMode(dev string, port int) string {
	dir := filepath.Join(sysRoot, "class/infiniband", dev, "ports", strconv.Itoa(port), "gid_attrs/types")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	mode := ""
	for _, e := range entries {
		t := strings.TrimSpace(readFile(filepath.Join(dir, e.Name())))
		switch {
		case strings.Contains(t, "v2"):
			return "v2"
		case strings.HasPrefix(t, "IB/RoCE v1"), strings.HasPrefix(t, "RoCE v1"):
			mode = "v1"
		}
	}
	return mode
}

// sysfsECN reads the mlx5 ECN knobs: /sys/class/net/<dev>/ecn/roce_np/enable/<prio>
// (notification point: the receiver sends CNPs) and roce_rp/enable/<prio>
// (reaction point: the sender slows down), plus the DSCP and priority CNPs
// are sent with. Returns the enabled priorities as comma lists.
func sysfsECN(netdev string) (np, rp string, cnpDSCP, cnpPrio int, ok bool) {
	base := filepath.Join(sysRoot, "class/net", netdev, "ecn")
	if _, err := os.Stat(base); err != nil {
		return "", "", -1, -1, false
	}
	on := func(kind string) string {
		var prios []string
		for p := 0; p < 8; p++ {
			if strings.TrimSpace(readFile(filepath.Join(base, kind, "enable", strconv.Itoa(p)))) == "1" {
				prios = append(prios, strconv.Itoa(p))
			}
		}
		if len(prios) == 0 {
			return "none"
		}
		return strings.Join(prios, ",")
	}
	cnpDSCP, cnpPrio = -1, -1
	if n, err := strconv.Atoi(strings.TrimSpace(readFile(filepath.Join(base, "roce_np/cnp_dscp")))); err == nil {
		cnpDSCP = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(readFile(filepath.Join(base, "roce_np/cnp_802p_prio")))); err == nil {
		cnpPrio = n
	}
	return on("roce_np"), on("roce_rp"), cnpDSCP, cnpPrio, true
}

var (
	dcbPrioPFCRe = regexp.MustCompile(`"prio-pfc"\s*:\s*\{([^}]*)\}`)
	dcbPairRe    = regexp.MustCompile(`"(\d+)"\s*:\s*(true|false|"on"|"off")`)
	dcbDscpRe    = regexp.MustCompile(`"dscp-prio"\s*:\s*\[((?:\s*\[\s*\d+\s*,\s*\d+\s*\]\s*,?)*)\]`)
	dcbPcpRe     = regexp.MustCompile(`"pcp-prio"\s*:\s*\[((?:\s*\[\s*\d+\s*,\s*\d+\s*\]\s*,?)*)\]`)
	dcbMapRe     = regexp.MustCompile(`\[\s*(\d+)\s*,\s*(\d+)\s*\]`)
)

// ParseDCBPFC reads `dcb -j pfc show dev X` into the list of priorities with
// PFC on ("3", "3,4") or "none"; ok is false when the output has no PFC block.
func ParseDCBPFC(out string) (string, bool) {
	m := dcbPrioPFCRe.FindStringSubmatch(out)
	if m == nil {
		return "", false
	}
	var on []int
	for _, p := range dcbPairRe.FindAllStringSubmatch(m[1], -1) {
		if p[2] == "true" || p[2] == `"on"` {
			n, _ := strconv.Atoi(p[1])
			on = append(on, n)
		}
	}
	return prioList(on), true
}

// ParseDCBApp reads `dcb -j app show dev X`: the DSCP to priority map
// ("26:3,48:6") and the trust mode it implies (dscp when a DSCP map exists,
// pcp when only a PCP map does, "" when neither).
func ParseDCBApp(out string) (dscpPrio, trust string) {
	if m := dcbDscpRe.FindStringSubmatch(out); m != nil {
		var pairs []string
		for _, p := range dcbMapRe.FindAllStringSubmatch(m[1], -1) {
			pairs = append(pairs, p[1]+":"+p[2])
		}
		if len(pairs) > 0 {
			return strings.Join(pairs, ","), "dscp"
		}
	}
	if m := dcbPcpRe.FindStringSubmatch(out); m != nil && dcbMapRe.MatchString(m[1]) {
		return "", "pcp"
	}
	return "", ""
}

// mlnxQos is what ParseMlnxQos reads from `mlnx_qos -i X`.
type mlnxQos struct {
	Trust, PFC, DSCPPrio string
}

var mlnxDscpRe = regexp.MustCompile(`(?m)^\s*dscp(\d+)\s*(.*?)\s*$`)

// ParseMlnxQos reads mlnx_qos output: the trust state, the PFC enabled row
// and the dscp2prio table (lines like "dscp26 ... prio:3" vary by version:
// both "prio:3" and a bare trailing number are accepted).
func ParseMlnxQos(out string) mlnxQos {
	var m mlnxQos
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(t, "Priority trust state:"); ok {
			m.Trust = strings.TrimSpace(v)
		}
		if strings.HasPrefix(t, "PFC configuration") {
			// The block lists "priority 0 1 2 3 4 5 6 7" then "enabled 0 0 0 1 0 0 0 0".
			for _, l := range lines[i+1 : min(i+6, len(lines))] {
				f := strings.Fields(l)
				if len(f) == 9 && f[0] == "enabled" {
					var on []int
					for p, v := range f[1:] {
						if v == "1" {
							on = append(on, p)
						}
					}
					m.PFC = prioList(on)
				}
			}
		}
	}
	var pairs []string
	for _, d := range mlnxDscpRe.FindAllStringSubmatch(out, -1) {
		rest := d[2]
		prio := ""
		if i := strings.Index(rest, "prio:"); i >= 0 {
			prio = strings.TrimSpace(rest[i+5:])
		} else if f := strings.Fields(rest); len(f) > 0 {
			prio = f[len(f)-1]
		}
		if n, err := strconv.Atoi(prio); err == nil && n != 0 {
			pairs = append(pairs, d[1]+":"+strconv.Itoa(n))
		}
	}
	m.DSCPPrio = strings.Join(pairs, ",")
	return m
}

func prioList(on []int) string {
	if len(on) == 0 {
		return "none"
	}
	sort.Ints(on)
	parts := make([]string, len(on))
	for i, n := range on {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}
