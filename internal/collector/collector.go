package collector
import("container/list";"context";"fmt";"log/slog";"math";"sort";"strings";"sync";"github.com/example/redfish-gpu-exporter/internal/cache";"github.com/example/redfish-gpu-exporter/internal/config";"github.com/example/redfish-gpu-exporter/internal/model";"github.com/example/redfish-gpu-exporter/internal/redfish")
type flight struct{done chan struct{}; snapshot model.Snapshot; err error}

// fairSem is a FIFO-ordered counting semaphore. A plain buffered-channel
// semaphore gives no ordering guarantee under sustained contention: when N
// goroutines race to send on a full channel, any of them may win as a slot
// frees up. With 15 independent targets retrying every ~5 minutes against a
// small cap, that let a handful of targets repeatedly win the race while the
// rest starved for 50+ minutes straight (observed live: 10 of 15 BMCs went
// unscraped for nearly an hour). Ticket order here is strict arrival order,
// so every waiter is guaranteed to reach the front eventually.
type fairSem struct{mu sync.Mutex;cur,max int;waiters *list.List}
func newFairSem(n int)*fairSem{return &fairSem{max:n,waiters:list.New()}}
func(s *fairSem) acquire(ctx context.Context)error{
	s.mu.Lock()
	if s.cur<s.max{s.cur++;s.mu.Unlock();return nil}
	ch:=make(chan struct{})
	elem:=s.waiters.PushBack(ch)
	s.mu.Unlock()
	select{
	case <-ch:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		s.waiters.Remove(elem)
		s.mu.Unlock()
		return ctx.Err()
	}
}
func(s *fairSem) release(){
	s.mu.Lock()
	defer s.mu.Unlock()
	if front:=s.waiters.Front();front!=nil{
		s.waiters.Remove(front)
		close(front.Value.(chan struct{}))
		return
	}
	s.cur--
}

type Collector struct{cfg config.Config;cache *cache.Cache;mu sync.Mutex;inFlight map[string]*flight;fleetSem *fairSem}
func New(c config.Config,cache *cache.Cache)*Collector{n:=c.GlobalConcurrency;if n<=0{n=8};return &Collector{cfg:c,cache:cache,inFlight:map[string]*flight{},fleetSem:newFairSem(n)}}
func(c *Collector) Collect(ctx context.Context,target string)(model.Snapshot,error){target=strings.TrimSpace(target);if target==""{return model.Snapshot{},fmt.Errorf("target parameter is required")};if s,ok:=c.cache.Get(target);ok{return s,nil};c.mu.Lock();if f,ok:=c.inFlight[target];ok{c.mu.Unlock();select{case<-f.done:return f.snapshot,f.err;case<-ctx.Done():return model.Snapshot{},ctx.Err()}};f:=&flight{done:make(chan struct{})};c.inFlight[target]=f;c.mu.Unlock()
	// Fleet-wide cap: bounds how many BMCs are being crawled at once across
	// the whole target list, independent of the per-BMC `concurrency` setting.
	// Without this, a cold cache (e.g. right after an exporter restart) lets
	// every target's crawl fire simultaneously and saturate the shared BMC/OOB
	// management network rather than just one controller's request queue.
	if e:=c.fleetSem.acquire(ctx);e==nil{
		f.snapshot,f.err=c.collect(ctx,target)
		c.fleetSem.release()
	}else{
		f.snapshot,f.err=model.Snapshot{},e
	}
	c.mu.Lock();delete(c.inFlight,target);close(f.done);c.mu.Unlock();return f.snapshot,f.err}
func(c *Collector) collect(ctx context.Context,target string)(model.Snapshot,error){ctx,cancel:=context.WithTimeout(ctx,c.cfg.ScrapeTimeout);defer cancel();cred:=c.cfg.Credentials(target);cl,e:=redfish.New(target,cred.Username,cred.Password,c.cfg.Timeout,c.cfg.InsecureSkipVerify,c.cfg.Retries);if e!=nil{return model.Snapshot{},e}
	// Per-host override for BMCs that can't tolerate the fleet-default
	// per-BMC concurrency — some embedded controllers (observed on a handful
	// of AMI MegaRAC-based GPU tray BMCs) apply an anti-hammering lockout
	// once enough concurrent Basic-Auth requests land in a short window,
	// denying the rest of that crawl (Security.1.0.AccessDenied) even though
	// earlier resources in the same crawl succeeded.
	conc:=c.cfg.Concurrency
	if cred.Concurrency>0{conc=cred.Concurrency}
	// Per-host override for BMCs whose resource graph is far larger than the
	// fleet default budget covers — NVIDIA HGX baseboard controllers expose
	// 40 Chassis members alone (per-GPU ERoT security co-processor, NVSwitch,
	// PCIeRetimer, PCIeSwitch, plus the real per-GPU chassis), several times
	// the resource count of a typical Dell/Lenovo/SuperMicro host. The BFS
	// crawl below visits links in alphabetical order, and "HGX_ERoT_GPU_SXM_*"
	// sorts before "HGX_GPU_SXM_*" — so the fleet-default max_resources
	// silently exhausts on ERoT/NVSwitch/PCIeRetimer chassis before ever
	// reaching the real GPU chassis' Sensors/ThermalSubsystem resources,
	// with no error logged (it's a budget cutoff, not a fetch failure) and
	// redfish_gpu_health/info still present (Processors are reached via
	// Systems, a separate, smaller subtree) while
	// redfish_gpu_temperature_celsius silently never appears.
	max:=c.cfg.MaxResources
	if cred.MaxResources>0{max=cred.MaxResources}
	// Per-host override for BMCs whose deep subtrees (e.g. individual CPU
	// cores under Systems/{id}/Processors/{cpu}/SubProcessors/{core}, one
	// resource per physical core) inflate the crawl far beyond what raising
	// max_resources alone can afford within scrape_timeout. GPU sensor
	// readings sit shallower (Chassis/{id}/Sensors/{reading}, depth 4) than
	// per-core CPU detail (depth 6) — capping depth for these hosts trades
	// that per-core/per-drive granularity away in exchange for actually
	// reaching GPU thermal data within budget.
	depth:=c.cfg.MaxDepth
	if cred.MaxDepth>0{depth=cred.MaxDepth}
	docs,e:=crawl(ctx,target,cl,max,depth,conc);if e!=nil{return model.Snapshot{},e};s:=normalize(docs);c.cache.Put(target,s);return s,nil}
func crawl(ctx context.Context,target string,c *redfish.Client,max,depth,concurrency int)([]map[string]any,error){
	// Resolve the service root explicitly. Returning this error is important:
	// a generic "no resources" response conceals certificate, credential, and
	// network faults that an operator needs to correct.
	root,err:=c.Get(ctx,"/redfish/v1/")
	if err!=nil{return nil,fmt.Errorf("get Redfish service root: %w",err)}
	type item struct{p string;d int}
	docs:=[]map[string]any{root}
	seen:=map[string]bool{"/redfish/v1/":true}
	var wave []item
	for _,p:=range links(root){if usefulPath(p)&&!seen[p]{seen[p]=true;wave=append(wave,item{p,1})}}
	// Fetch one BFS level at a time, up to `concurrency` requests in flight per
	// level. A single BMC's embedded controller is slow (100s of ms/request)
	// but often fragile under high concurrency, so this trades wall-clock time
	// for a bounded, per-BMC-friendly request rate rather than going fully
	// serial (too slow once the crawl covers Systems/Processors/Memory/Storage)
	// or fully parallel (risks overloading the BMC firmware).
	for len(wave)>0&&len(docs)<max{
		if err:=ctx.Err();err!=nil{return nil,fmt.Errorf("Redfish collection deadline after %d resources: %w",len(docs),err)}
		if len(docs)+len(wave)>max{wave=wave[:max-len(docs)]}
		results:=make([]map[string]any,len(wave))
		sem:=make(chan struct{},concurrency)
		var wg sync.WaitGroup
		for i,it:=range wave{
			wg.Add(1)
			sem<-struct{}{}
			go func(i int,p string){defer wg.Done();defer func(){<-sem}();if d,e:=c.Get(ctx,p);e==nil{results[i]=d}else{slog.Warn("redfish resource fetch failed",
				"target",target,"path",p,"error",e)}}(i,it.p)
		}
		wg.Wait()
		// A wave whose fetches all failed (e.g. the caller's request context
		// was canceled mid-crawl) yields no results and thus no next-wave
		// links — the outer loop condition then exits normally, and without
		// this check `docs` (root doc only, or whatever completed before the
		// cancellation) would be returned as if it were a complete, healthy
		// crawl. That previously surfaced as redfish_scrape_success=1 with
		// almost no metrics for a target whose crawl was actually aborted.
		if err:=ctx.Err();err!=nil{return nil,fmt.Errorf("Redfish collection deadline after %d resources: %w",len(docs),err)}
		var next []item
		for i,it:=range wave{
			d:=results[i];if d==nil{continue}
			docs=append(docs,d)
			if len(docs)>=max{break}
			if it.d<depth{for _,p:=range links(d){if usefulPath(p)&&!seen[p]{seen[p]=true;next=append(next,item{p,it.d+1})}}}
		}
		wave=next
	}
	if len(docs)==0{return nil,fmt.Errorf("Redfish service root unavailable")};return docs,nil
}
func links(v any)[]string{set:=map[string]bool{};var walk func(any);walk=func(x any){switch z:=x.(type){case map[string]any:for k,v:=range z{if k=="@odata.id"{if s,ok:=v.(string);ok&&strings.HasPrefix(s,"/"){// A # fragment identifies data inside the parent JSON document, not an HTTP resource.
			if i:=strings.IndexByte(s,'#');i>=0{s=s[:i]};if s!=""{set[s]=true}}};walk(v)};case []any:for _,v:=range z{walk(v)}}};walk(v);out:=make([]string,0,len(set));for x:=range set{if !strings.Contains(x,"$metadata"){out=append(out,x)}};sort.Strings(out);return out}
func usefulPath(p string)bool{
	if p=="/redfish/v1/Chassis"||p=="/redfish/v1/Systems"{return true}
	if strings.HasPrefix(p,"/redfish/v1/Chassis/"){
		rest:=strings.TrimPrefix(p,"/redfish/v1/Chassis/")
		if !strings.Contains(rest,"/"){return true} // /Chassis/{id}
		// GPU baseboards/trays are frequently modeled as their own Chassis
		// (not under Systems/{id}/Processors), so Processors must be crawled
		// here too or GPU health/inventory is silently never discovered.
		return strings.Contains(p,"/Thermal")||strings.Contains(p,"/Sensors")||strings.Contains(p,"/Power")||strings.Contains(p,"/Processors")
	}
	if strings.HasPrefix(p,"/redfish/v1/Systems/"){
		rest:=strings.TrimPrefix(p,"/redfish/v1/Systems/")
		if !strings.Contains(rest,"/"){return true} // /Systems/{id}
		return strings.Contains(p,"/Processors")||strings.Contains(p,"/Memory")||strings.Contains(p,"/Storage")||strings.Contains(p,"/Drives")
	}
	return false
}
func normalize(ds []map[string]any)model.Snapshot{
	s:=model.Snapshot{Vendor:"unknown",Resources:len(ds)}
	for _,d:=range ds{if s.Vendor=="unknown"{s.Vendor=first(str(d,"Manufacturer"),str(d,"Vendor"),"unknown")}}
	// Some BMC firmware (observed on this Lenovo XCC fleet) never exposes GPUs
	// as Processor/Accelerator resources at all — the only Redfish-visible GPU
	// signal is the PCIe slot temperature sensors under Chassis/Thermal. Fall
	// back to deriving redfish_gpu_health from a sensor's own Status.Health,
	// but only when no real GPU Processor resource was found anywhere in this
	// snapshot, so vendors that do report proper GPU Processors never get
	// double-counted.
	hasGPUProcessor:=false
	for _,d:=range ds{if strings.Contains(str(d,"@odata.type"),"#Processor.")&&isGPUProcessor(d){hasGPUProcessor=true;break}}
	for _,d:=range ds{
		walkGPUTemperatures(d,map[string]string{"name":label(str(d,"Name"),"unnamed")},&s.Metrics,!hasGPUProcessor)
		switch t:=str(d,"@odata.type");{
		case strings.Contains(t,"#ComputerSystem."):
			systemHealthMetric(d,&s.Metrics)
		case strings.Contains(t,"#Processor."):
			processorMetrics(d,&s.Metrics)
		case strings.Contains(t,"#Memory."):
			memoryMetric(d,&s.Metrics)
		case strings.Contains(t,"#Drive."):
			driveMetric(d,&s.Metrics)
		case strings.Contains(t,"#Power."):
			powerMetrics(d,&s.Metrics)
		case strings.Contains(t,"#Thermal."):
			thermalMetrics(d,&s.Metrics)
		}
	}
	for i:=range s.Metrics{s.Metrics[i].Labels["vendor"]=s.Vendor}
	return s
}
func isGPU(d map[string]any)bool{return strings.Contains(strings.ToLower(str(d,"DeviceType")+" "+str(d,"Model")+" "+str(d,"Name")),"gpu")||strings.Contains(strings.ToLower(str(d,"Manufacturer")),"nvidia")}
// An explicit ProcessorType is authoritative when present — NVIDIA also
// ships non-GPU parts (e.g. the HGX baseboard's ERoT FPGA) that the looser
// Name/Model/Manufacturer heuristic below would otherwise misclassify as a
// GPU purely because Manufacturer=="NVIDIA". Only fall back to that
// heuristic when the resource doesn't declare a ProcessorType at all.
func isGPUProcessor(d map[string]any)bool{if pt:=str(d,"ProcessorType");pt!=""{return strings.EqualFold(pt,"GPU")};return isGPU(d)}
func health(d map[string]any)(float64,bool){st,ok:=d["Status"].(map[string]any);if !ok{return 0,false};h,ok:=st["Health"].(string);if !ok||h==""{return 0,false};if strings.EqualFold(h,"OK"){return 1,true};return 0,true}
func systemHealthMetric(d map[string]any,out *[]model.Metric){h,ok:=health(d);if !ok{return};name:=label(str(d,"Name"),label(str(d,"Id"),"System"));*out=append(*out,metric("redfish_system_health","System health status (1=OK)",map[string]string{"name":name},h))}
func processorMetrics(d map[string]any,out *[]model.Metric){
	h,ok:=health(d);if !ok{return}
	// Name alone is not a reliable discriminator: some OEM firmware gives every
	// socket/GPU on a board the identical display Name, so multiple resources
	// would otherwise collapse onto the same series and silently drop samples
	// (Prometheus "different value but same timestamp" — can mask a real failure).
	name:=label(str(d,"Name"),str(d,"Id"))
	labels:=map[string]string{"name":name,"id":str(d,"Id")}
	if isGPUProcessor(d){
		*out=append(*out,metric("redfish_gpu_health","GPU health status (1=OK)",labels,h))
		info:=copyLabels(labels)
		if v:=str(d,"Model");v!=""{info["model"]=v}
		if v:=str(d,"Manufacturer");v!=""{info["manufacturer"]=v}
		*out=append(*out,metric("redfish_gpu_info","GPU inventory information",info,1))
		return
	}
	*out=append(*out,metric("redfish_cpu_health","CPU health status (1=OK)",labels,h))
}
func memoryMetric(d map[string]any,out *[]model.Metric){h,ok:=health(d);if !ok{return};name:=label(str(d,"Name"),str(d,"Id"));*out=append(*out,metric("redfish_memory_health","Memory module health status (1=OK)",map[string]string{"name":name,"id":str(d,"Id")},h))}
func driveMetric(d map[string]any,out *[]model.Metric){h,ok:=health(d);if !ok{return};name:=label(str(d,"Name"),str(d,"Id"));*out=append(*out,metric("redfish_disk_health","Disk drive health status (1=OK)",map[string]string{"name":name,"id":str(d,"Id")},h))}
func powerMetrics(d map[string]any,out *[]model.Metric){
	if psus,ok:=d["PowerSupplies"].([]any);ok{
		for _,item:=range psus{
			p,ok:=item.(map[string]any);if !ok{continue}
			h,ok:=health(p);if !ok{continue}
			name:=label(str(p,"Name"),label(str(p,"MemberId"),"PSU"))
			*out=append(*out,metric("redfish_psu_health","Power supply health status (1=OK)",map[string]string{"name":name,"id":str(p,"MemberId")},h))
		}
	}
	if controls,ok:=d["PowerControl"].([]any);ok{
		for _,item:=range controls{
			p,ok:=item.(map[string]any);if !ok{continue}
			watts,num:=number(p["PowerConsumedWatts"]);if !num{continue}
			name:=label(str(p,"Name"),"Total")
			*out=append(*out,metric("redfish_power_watts","Power consumption in Watts",map[string]string{"name":name},watts))
		}
	}
}
func thermalMetrics(d map[string]any,out *[]model.Metric){
	if fans,ok:=d["Fans"].([]any);ok{
		for _,item:=range fans{
			f,ok:=item.(map[string]any);if !ok{continue}
			reading,num:=number(f["Reading"]);if !num{continue}
			name:=label(str(f,"Name"),"Fan")
			*out=append(*out,metric("redfish_fan_speed_rpm","Fan speed in RPM",map[string]string{"name":name},reading))
		}
	}
	if temps,ok:=d["Temperatures"].([]any);ok{
		for _,item:=range temps{
			t,ok:=item.(map[string]any);if !ok{continue}
			reading,num:=number(t["ReadingCelsius"]);if !num{reading,num=number(t["Reading"])};if !num{continue}
			name:=label(str(t,"Name"),"Sensor")
			*out=append(*out,metric("redfish_temperature_celsius","Temperature sensor reading in Celsius",map[string]string{"name":name},reading))
		}
	}
}
func walkGPUTemperatures(v any,labels map[string]string,out *[]model.Metric,emitHealthFallback bool){m,ok:=v.(map[string]any);if !ok{return};n:=str(m,"Name");if n!=""{labels=copyLabels(labels);labels["name"]=n};reading,num:=number(m["Reading"]);if !num{reading,num=number(m["ReadingCelsius"])};units:=strings.ToLower(str(m,"ReadingUnits"));if num&&(strings.Contains(units,"celsius")||strings.Contains(units,"degree")||m["ReadingCelsius"]!=nil)&&isGPUTemperatureSensor(labels["name"]){*out=append(*out,metric("redfish_gpu_temperature_celsius","GPU temperature in Celsius",labels,reading));if emitHealthFallback{if h,ok:=health(m);ok{*out=append(*out,metric("redfish_gpu_health","GPU health status (1=OK), derived from PCIe temperature sensor Status — no GPU Processor resource reported by this BMC",labels,h))}}};for _,v:=range m{switch x:=v.(type){case map[string]any:walkGPUTemperatures(x,labels,out,emitHealthFallback);case []any:for _,z:=range x{walkGPUTemperatures(z,labels,out,emitHealthFallback)}}}}
func isGPUTemperatureSensor(name string)bool{
	n:=strings.ToLower(name)
	// NVIDIA HGX baseboards report several sibling readings per physical GPU
	// under names containing "gpu"/"pcie" that are not a live temperature of
	// that GPU: *_TEMP_LIMIT is a configured throttle threshold (not a
	// reading — counting it as one inflates both the temperature series and
	// any downstream "how many GPUs" count, and could misreport a static
	// threshold as the fleet's hottest live GPU temperature), and
	// PCIe Retimer/Switch chips are PCIe fabric components, not GPUs.
	if strings.Contains(n,"temp_limit")||strings.Contains(n,"retimer")||strings.Contains(n,"pcieswitch")||strings.Contains(n,"pcie_switch"){return false}
	return strings.Contains(n,"gpu")||strings.Contains(n,"pcie")
}
func str(m map[string]any,k string)string{if x,ok:=m[k].(string);ok{return x};return ""};func number(x any)(float64,bool){switch v:=x.(type){case float64:return v,!math.IsNaN(v);case int:return float64(v),true};return 0,false};func label(s,d string)string{if s==""{return d};return s};func first(values ...string)string{for _,v:=range values{if v!=""{return v}};return ""};func copyLabels(l map[string]string)map[string]string{r:=map[string]string{};for k,v:=range l{r[k]=v};return r};func metric(n,h string,l map[string]string,v float64)model.Metric{return model.Metric{n,h,copyLabels(l),v}}
