package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Credential struct { Username string `yaml:"username"`; Password string `yaml:"password"`; PasswordEnv string `yaml:"password_env"`; Concurrency int `yaml:"concurrency"` }
type Raw struct { Hosts map[string]Credential `yaml:"hosts"`; Timeout string `yaml:"timeout"`; ScrapeTimeout string `yaml:"scrape_timeout"`; Retries int `yaml:"retries"`; InsecureSkipVerify bool `yaml:"insecure_skip_verify"`; CacheTTL string `yaml:"cache_ttl"`; MaxResources int `yaml:"max_resources"`; MaxDepth int `yaml:"max_depth"`; Concurrency int `yaml:"concurrency"`; GlobalConcurrency int `yaml:"global_concurrency"` }
type Config struct { Hosts map[string]Credential; Timeout, ScrapeTimeout, CacheTTL time.Duration; Retries, MaxResources, MaxDepth, Concurrency, GlobalConcurrency int; InsecureSkipVerify bool }

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path); if err != nil { return Config{}, err }
	var r Raw; if err := yaml.Unmarshal(b, &r); err != nil { return Config{}, fmt.Errorf("parse %s: %w", path, err) }
	c := Config{Hosts:r.Hosts, Timeout:30*time.Second, ScrapeTimeout:60*time.Second, CacheTTL:5*time.Minute, Retries:r.Retries, MaxResources:r.MaxResources, MaxDepth:r.MaxDepth, Concurrency:r.Concurrency, GlobalConcurrency:r.GlobalConcurrency, InsecureSkipVerify:r.InsecureSkipVerify}
	if c.Retries == 0 { c.Retries=2 }; if c.MaxResources == 0 { c.MaxResources=500 }; if c.MaxDepth == 0 { c.MaxDepth=8 }; if c.Concurrency == 0 { c.Concurrency=4 }; if c.GlobalConcurrency == 0 { c.GlobalConcurrency=8 }
	if r.Timeout != "" { if c.Timeout,err=time.ParseDuration(r.Timeout);err!=nil{return c,err} }; if r.ScrapeTimeout!="" {if c.ScrapeTimeout,err=time.ParseDuration(r.ScrapeTimeout);err!=nil{return c,err}}; if r.CacheTTL!="" {if c.CacheTTL,err=time.ParseDuration(r.CacheTTL);err!=nil{return c,err}}
	def,ok:=c.Hosts["default"]; if !ok || def.Username=="" || (def.Password=="" && def.PasswordEnv=="") { return c,fmt.Errorf("hosts.default needs username and password_env (or password)") }
	if def.PasswordEnv!="" { def.Password=os.Getenv(def.PasswordEnv) }
	c.Hosts["default"]=def
	for host, v := range c.Hosts {
		if host=="default" { continue }
		if v.PasswordEnv!="" { v.Password=os.Getenv(v.PasswordEnv) }
		// A per-host block may set only `concurrency` (e.g. to throttle a BMC
		// that rate-limits/locks out under this exporter's default per-host
		// concurrency) without repeating credentials — fall back to default's.
		if v.Username=="" { v.Username=def.Username }
		if v.Password=="" { v.Password=def.Password }
		if v.Username=="" || v.Password=="" { return c,fmt.Errorf("credentials for %s are incomplete",host) }
		c.Hosts[host]=v
	}
	return c,nil
}
func (c Config) Credentials(host string) Credential { host=strings.TrimSpace(host); if v,ok:=c.Hosts[host];ok{return v}; return c.Hosts["default"] }
