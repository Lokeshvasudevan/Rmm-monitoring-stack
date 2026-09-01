package metrics
import("fmt";"net/http";"sort";"strconv";"strings";"github.com/example/redfish-gpu-exporter/internal/collector";"github.com/prometheus/client_golang/prometheus";"github.com/prometheus/client_golang/prometheus/promhttp")
type Handler struct{collector *collector.Collector; self *prometheus.Registry; requests prometheus.Counter; inFlight prometheus.Gauge}
func New(c *collector.Collector)*Handler{r:=prometheus.NewRegistry();q:=prometheus.NewCounter(prometheus.CounterOpts{Name:"redfish_exporter_requests_total",Help:"Total Redfish scrape requests"});f:=prometheus.NewGauge(prometheus.GaugeOpts{Name:"redfish_exporter_scrapes_in_flight",Help:"Redfish collections currently running"});r.MustRegister(q,f);return &Handler{c,r,q,f}}
func(h *Handler)Self()http.Handler{return promhttp.HandlerFor(h.self,promhttp.HandlerOpts{})}
func(h *Handler)Redfish(w http.ResponseWriter,r *http.Request){
	h.requests.Inc();h.inFlight.Inc();defer h.inFlight.Dec()
	target:=r.URL.Query().Get("target")
	s,e:=h.collector.Collect(r.Context(),target)
	if e!=nil{http.Error(w,e.Error(),http.StatusBadGateway);return}
	w.Header().Set("Content-Type","text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintln(w,"# HELP redfish_scrape_success Whether the Redfish scrape succeeded (1=yes)")
	fmt.Fprintln(w,"# TYPE redfish_scrape_success gauge")
	fmt.Fprintln(w,"redfish_scrape_success 1")
	// Configured GPU count from inventory (passed through by Prometheus as
	// __param_gpu_count), not derived from this crawl's telemetry. Some BMC
	// firmware only exposes a handful of zone/aggregate GPU temperature
	// sensors rather than one per physical GPU, so counting sensor series
	// undercounts real GPUs on those hosts — the inventory value is
	// authoritative regardless of what a given BMC's Redfish implementation
	// chooses to expose. Emitted even though this metric is independent of
	// the crawl above, so it's present as long as the target is configured,
	// including while that BMC is otherwise unreachable.
	if gc:=r.URL.Query().Get("gpu_count");gc!=""{
		if v,pe:=strconv.ParseFloat(gc,64);pe==nil{
			fmt.Fprintln(w,"# HELP redfish_gpu_count_configured Number of physical GPUs configured for this server in inventory")
			fmt.Fprintln(w,"# TYPE redfish_gpu_count_configured gauge")
			fmt.Fprintf(w,"redfish_gpu_count_configured{instance=\"%s\"} %s\n",target,strconv.FormatFloat(v,'f',-1,64))
		}
	}
	for _,m:=range s.Metrics{fmt.Fprintf(w,"# HELP %s %s\n# TYPE %s gauge\n%s%s %s\n",m.Name,m.Help,m.Name,m.Name,formatLabels(m.Labels),strconv.FormatFloat(m.Value,'f',-1,64))}
}
func formatLabels(l map[string]string)string{if len(l)==0{return ""};ks:=make([]string,0,len(l));for k:=range l{ks=append(ks,k)};sort.Strings(ks);p:=make([]string,0,len(ks));for _,k:=range ks{p=append(p,k+"=\""+strings.ReplaceAll(strings.ReplaceAll(l[k],"\\","\\\\"),"\"","\\\"")+"\"")};return "{"+strings.Join(p,",")+"}"}
