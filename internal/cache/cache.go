package cache
import ("sync"; "time"; "github.com/example/redfish-gpu-exporter/internal/model")
type item struct { snapshot model.Snapshot; expires time.Time }
type Cache struct { mu sync.RWMutex; ttl time.Duration; values map[string]item }
func New(ttl time.Duration)*Cache{return &Cache{ttl:ttl,values:map[string]item{}}}
func(c *Cache)Get(k string)(model.Snapshot,bool){c.mu.RLock(); v,ok:=c.values[k];c.mu.RUnlock();return v.snapshot,ok&&time.Now().Before(v.expires)}
func(c *Cache)Put(k string,v model.Snapshot){c.mu.Lock();c.values[k]=item{v,time.Now().Add(c.ttl)};c.mu.Unlock()}
