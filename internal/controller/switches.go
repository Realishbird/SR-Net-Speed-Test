package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

var switchNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Switch management from the UI and CLI. Changes are written to the
// controller config, the SNMP poller picks them up at once, and the gnmic
// config is rewritten; a systemd path unit (installed by
// deploy/install-controller.sh) restarts gnmic when that file changes.
func (s *Server) routesSwitches(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/switches", s.view(s.listSwitches))
	mux.HandleFunc("PUT /api/v1/switches/{name}", s.admin(s.putSwitch))
	mux.HandleFunc("DELETE /api/v1/switches/{name}", s.admin(s.deleteSwitch))
	mux.HandleFunc("POST /api/v1/switches/{name}/test", s.admin(s.testSwitch))
}

func (s *Server) switchList() []SwitchConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SwitchConfig(nil), s.cfg.Switches...)
}

func (s *Server) findSwitch(name string) (SwitchConfig, bool) {
	for _, sw := range s.switchList() {
		if sw.Name == name {
			return sw, true
		}
	}
	return SwitchConfig{}, false
}

// applySwitches stores a new switch list and propagates it to the collectors.
func (s *Server) applySwitches(ctx context.Context, list []SwitchConfig, action string, detail map[string]any) error {
	s.mu.Lock()
	s.cfg.Switches = list
	cfg := s.cfg
	s.mu.Unlock()
	if err := cfg.SaveBack(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	s.snmp.set(list)
	s.writeGnmic(ctx)
	s.store.Audit(ctx, action, detail)
	return nil
}

// mergeSwitch builds the stored config for a request, keeping secrets that
// the request left blank.
func mergeSwitch(req api.SwitchRequest, old SwitchConfig, hadOld bool) (SwitchConfig, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.Address = strings.TrimSpace(req.Address)
	if !switchNameRe.MatchString(req.Name) {
		return SwitchConfig{}, errors.New("name must be 1-63 letters, digits, dots, dashes or underscores")
	}
	if req.Address == "" {
		return SwitchConfig{}, errors.New("address is required")
	}
	if !req.SNMPEnabled && !req.GNMIEnabled {
		return SwitchConfig{}, errors.New("enable SNMP, gNMI or both")
	}
	sw := SwitchConfig{Name: req.Name, Address: req.Address}
	if req.SNMPEnabled {
		sw.SNMPVersion = req.SNMPVersion
		if sw.SNMPVersion != "1" {
			sw.SNMPVersion = "2c"
		}
		sw.SNMPCommunity = req.SNMPCommunity
		if sw.SNMPCommunity == "" && hadOld {
			sw.SNMPCommunity = old.SNMPCommunity
		}
		if sw.SNMPCommunity == "" {
			return SwitchConfig{}, errors.New("SNMP community is required")
		}
	}
	if req.GNMIEnabled {
		sw.GNMIUser = strings.TrimSpace(req.GNMIUser)
		if sw.GNMIUser == "" {
			return SwitchConfig{}, errors.New("gNMI username is required")
		}
		sw.GNMIPassword = req.GNMIPassword
		if sw.GNMIPassword == "" && hadOld {
			sw.GNMIPassword = old.GNMIPassword
		}
		if sw.GNMIPassword == "" {
			return SwitchConfig{}, errors.New("gNMI password is required")
		}
		sw.GNMIPort = req.GNMIPort
		if sw.GNMIPort <= 0 || sw.GNMIPort > 65535 {
			sw.GNMIPort = 8080
		}
		sw.GNMITLS = req.GNMITLS
	}
	return sw, nil
}

func (s *Server) switchView(sw SwitchConfig, snmp map[string]snmpStatus, gnmiSeries map[string]int, gnmiErr error) api.Switch {
	v := api.Switch{Name: sw.Name, Address: sw.Address, SNMPVersion: sw.SNMPVersion, HasSNMP: sw.SNMPCommunity != "",
		GNMIPort: sw.GNMIPort, GNMIUser: sw.GNMIUser, HasGNMIPassword: sw.GNMIPassword != "", GNMITLS: sw.GNMITLS}
	if v.HasSNMP {
		p := &api.SwitchProbe{Message: "waiting for the first poll"}
		if st, ok := snmp[sw.Name]; ok {
			at := st.At
			p.At, p.Ports, p.OK, p.Message = &at, st.Ports, st.Err == "", st.Err
			if p.OK {
				p.Message = fmt.Sprintf("%d interfaces", st.Ports)
			}
		}
		v.SNMP = p
	}
	if sw.GNMIUser != "" {
		p := &api.SwitchProbe{}
		switch {
		case gnmiErr != nil:
			p.Message = "could not query VictoriaMetrics: " + gnmiErr.Error()
		case gnmiSeriesFor(gnmiSeries, sw) > 0:
			p.OK, p.Series = true, gnmiSeriesFor(gnmiSeries, sw)
			p.Message = fmt.Sprintf("%d series in the last 2 min", p.Series)
		default:
			p.Message = "no data in the last 2 min (check the switch's telemetry service and the gnmic journal)"
		}
		v.GNMI = p
	}
	return v
}

func (s *Server) listSwitches(w http.ResponseWriter, r *http.Request) {
	list := s.switchList()
	series, err := s.gnmiSeries(r.Context())
	out := make([]api.Switch, 0, len(list))
	snmp := s.snmp.statuses()
	for _, sw := range list {
		v := s.switchView(sw, snmp, series, err)
		if s.health != nil {
			s.health.mu.Lock()
			if h, ok := s.health.last[sw.Name]; ok {
				hc := h
				v.Health = &hc
			}
			s.health.mu.Unlock()
		}
		out = append(out, v)
	}
	writeJSON(w, out)
}

func (s *Server) putSwitch(w http.ResponseWriter, r *http.Request) {
	var req api.SwitchRequest
	if !readJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	if req.Name == "" {
		req.Name = name
	}
	old, had := s.findSwitch(name)
	sw, err := mergeSwitch(req, old, had)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	var list []SwitchConfig
	for _, x := range s.switchList() {
		if x.Name != name && x.Name != sw.Name {
			list = append(list, x)
		}
	}
	list = append(list, sw)
	if err := s.applySwitches(r.Context(), list, "switch.save", map[string]any{"name": sw.Name, "address": sw.Address, "snmp": sw.SNMPCommunity != "", "gnmi": sw.GNMIUser != ""}); err != nil {
		serverError(w, err)
		return
	}
	// Poll straight away so the UI shows a verdict instead of "waiting".
	snmp := map[string]snmpStatus{}
	if sw.SNMPCommunity != "" {
		snmp[sw.Name] = s.snmp.pollNow(sw)
	}
	series, gerr := s.gnmiSeries(r.Context())
	writeJSON(w, s.switchView(sw, snmp, series, gerr))
}

func (s *Server) deleteSwitch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var list []SwitchConfig
	found := false
	for _, x := range s.switchList() {
		if x.Name == name {
			found = true
			continue
		}
		list = append(list, x)
	}
	if !found {
		httpError(w, http.StatusNotFound, "no switch named "+name)
		return
	}
	if err := s.applySwitches(r.Context(), list, "switch.delete", map[string]any{"name": name}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testSwitch(w http.ResponseWriter, r *http.Request) {
	sw, ok := s.findSwitch(r.PathValue("name"))
	if !ok {
		httpError(w, http.StatusNotFound, "no switch named "+r.PathValue("name"))
		return
	}
	snmp := map[string]snmpStatus{}
	if sw.SNMPCommunity != "" {
		snmp[sw.Name] = s.snmp.pollNow(sw)
	}
	series, err := s.gnmiSeries(r.Context())
	writeJSON(w, s.switchView(sw, snmp, series, err))
}

// gnmiSeries asks VictoriaMetrics how many srnet_gnmi_* series each switch
// produced in the last two minutes, which is the only cheap way to tell
// whether gnmic's subscription to a switch is alive.
// gnmiSeriesFor returns the series count gnmic reported for a switch, matching
// the lower-cased target name first and the address (with and without the
// gNMI port) as a fallback.
func gnmiSeriesFor(series map[string]int, sw SwitchConfig) int {
	if n := series[strings.ToLower(sw.Name)]; n > 0 {
		return n
	}
	addr := strings.ToLower(sw.Address)
	if n := series[fmt.Sprintf("%s:%d", addr, sw.GNMIPort)]; n > 0 {
		return n
	}
	return series[addr]
}

func (s *Server) gnmiSeries(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	if s.metrics.vmURL == "" {
		return out, nil
	}
	q := `count by (source) (last_over_time({__name__=~"srnet_gnmi_.+"}[2m]))`
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.metrics.vmURL+"/api/v1/query?query="+url.QueryEscape(q), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	var body struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return out, err
	}
	for _, r := range body.Data.Result {
		if len(r.Value) != 2 {
			continue
		}
		n := 0
		fmt.Sscanf(fmt.Sprint(r.Value[1]), "%d", &n)
		// gnmic labels samples with the target name as it read it from its
		// config, which viper lower-cases ("z9864F" becomes "z9864f"), or with
		// host:port when the target is unnamed. Keys are stored lower-cased and
		// looked up the same way by gnmiSeriesFor.
		src := strings.ToLower(r.Metric["source"])
		name, _, _ := strings.Cut(src, ":")
		out[src] += n
		if name != src {
			out[name] += n
		}
	}
	return out, nil
}
