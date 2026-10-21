package agent

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ModuleDOM is the transceiver reading from `ethtool -m`.
type ModuleDOM struct {
	TempC      *float64
	VoltageV   *float64
	TxBiasMa   []float64 // per channel
	TxPowerMw  []float64
	RxPowerMw  []float64
	PowerW     *float64
	PowerRated bool // true when PowerW is a static rating, not a live reading
}

var (
	domTempRe     = regexp.MustCompile(`(?im)^\s*Module temperature\s*:\s*([-+]?\d+(?:\.\d+)?)`)
	domVoltRe     = regexp.MustCompile(`(?im)^\s*Module voltage\s*:\s*([-+]?\d+(?:\.\d+)?)`)
	domPowerRe    = regexp.MustCompile(`(?im)^\s*(?:Module\s+)?Power consumption\s*:\s*([-+]?\d+(?:\.\d+)?)`)
	domMaxPowerRe = regexp.MustCompile(`(?im)^\s*Maximum power\s*:\s*([-+]?\d+(?:\.\d+)?)`)
	domExtIDRe    = regexp.MustCompile(`(?im)^\s*Extended identifier description\s*:\s*([-+]?\d+(?:\.\d+)?)\s*W\s+max\.\s*Power consumption`)
	domBiasRe     = regexp.MustCompile(`(?im)^\s*Laser (?:tx )?bias current[^:]*:\s*([-+]?\d+(?:\.\d+)?)\s*mA`)
	domTxPowRe    = regexp.MustCompile(`(?im)^\s*Transmit (?:avg )?optical power[^:]*:\s*([-+]?\d+(?:\.\d+)?)\s*mW`)
	domRxPowRe    = regexp.MustCompile(`(?im)^\s*(?:Rcvr signal|Receiver signal) (?:avg|average) optical power[^:]*:\s*([-+]?\d+(?:\.\d+)?)\s*mW`)
)

// ParseModuleDOM reads the values the PowerShell tool reported, plus per-channel optics.
// A missing power-consumption line falls back to the module's rated maximum,
// flagged as PowerRated so it is never shown as a live measurement.
func ParseModuleDOM(text string) ModuleDOM {
	var d ModuleDOM
	d.TempC = firstFloat(domTempRe, text)
	d.VoltageV = firstFloat(domVoltRe, text)
	if v := firstFloat(domPowerRe, text); v != nil {
		d.PowerW = v
	} else if v := firstFloat(domMaxPowerRe, text); v != nil {
		d.PowerW, d.PowerRated = v, true
	} else if v := firstFloat(domExtIDRe, text); v != nil {
		d.PowerW, d.PowerRated = v, true
	}
	d.TxBiasMa = allFloats(domBiasRe, text)
	d.TxPowerMw = allFloats(domTxPowRe, text)
	d.RxPowerMw = allFloats(domRxPowRe, text)
	return d
}

func firstFloat(re *regexp.Regexp, text string) *float64 {
	m := re.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return nil
	}
	return &v
}

func allFloats(re *regexp.Regexp, text string) []float64 {
	var out []float64
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// ethtoolModule runs `ethtool -m`, trying the narrowly scoped sudo rule first
// because some drivers return success with no EEPROM data when unprivileged.
func ethtoolModule(ctx context.Context, netdev string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "sudo", "-n", "/usr/sbin/ethtool", "-m", netdev).Output(); err == nil && len(out) > 0 {
		return string(out)
	}
	out, _ := exec.CommandContext(ctx, "ethtool", "-m", netdev).Output()
	return string(out)
}

// PerftestResult is the summary row of ib_write_bw / ib_read_bw with --report_gbits.
type PerftestResult struct {
	Bytes       int64
	Iterations  int64
	PeakGbps    float64
	AvgGbps     float64
	MsgRateMpps float64
	OK          bool
}

// ParsePerftest finds the numeric row under the "#bytes #iterations BW peak..." header.
func ParsePerftest(out string) PerftestResult {
	var r PerftestResult
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "#bytes") || !strings.Contains(line, "BW average") {
			continue
		}
		for _, row := range lines[i+1:] {
			f := strings.Fields(row)
			if len(f) < 5 {
				continue
			}
			nums := make([]float64, 5)
			ok := true
			for k := 0; k < 5; k++ {
				v, err := strconv.ParseFloat(f[k], 64)
				if err != nil {
					ok = false
					break
				}
				nums[k] = v
			}
			if !ok {
				continue
			}
			r = PerftestResult{
				Bytes:       int64(nums[0]),
				Iterations:  int64(nums[1]),
				PeakGbps:    nums[2],
				AvgGbps:     nums[3],
				MsgRateMpps: nums[4],
				OK:          true,
			}
			break
		}
	}
	return r
}

// netdevStatKeep selects the `ethtool -S` counters worth recording: drops, errors, pauses, FEC.
var netdevStatKeep = regexp.MustCompile(`(?i)(pause|discard|drop|err|fec|symbol|out_of_buffer|buf_discard|ecn|cnp)`)

// ParseEthtoolStats parses `ethtool -S` output, keeping only the counters in netdevStatKeep.
func ParseEthtoolStats(out string) map[string]float64 {
	stats := map[string]float64{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" || strings.Contains(k, " ") || !netdevStatKeep.MatchString(k) {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			continue
		}
		stats[k] = f
	}
	return stats
}
