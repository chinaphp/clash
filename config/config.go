package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Dreamacro/clash/adapters/remote"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/observable"
	R "github.com/Dreamacro/clash/rules"

	log "github.com/sirupsen/logrus"
	"gopkg.in/ini.v1"
)

var (
	config *Config
	once   sync.Once
)

// General config
type General struct {
	Port      int
	SocksPort int
	RedirPort int
	AllowLan  bool
	Mode      Mode
	LogLevel  C.LogLevel
}

// ProxyConfig is update proxy schema
type ProxyConfig struct {
	Port      *int
	SocksPort *int
	RedirPort *int
	AllowLan  *bool
}

// Config is clash config manager
type Config struct {
	general    *General
	rules      []C.Rule
	proxies    map[string]C.Proxy
	lastUpdate time.Time

	event      chan<- interface{}
	reportCh   chan interface{}
	observable *observable.Observable
}

// Event is event of clash config
type Event struct {
	Type    string
	Payload interface{}
}

// Subscribe config stream
func (c *Config) Subscribe() observable.Subscription {
	sub, _ := c.observable.Subscribe()
	return sub
}

// Report return a channel for collecting report message
func (c *Config) Report() chan<- interface{} {
	return c.reportCh
}

func (c *Config) readConfig() (*ini.File, error) {
	if _, err := os.Stat(C.ConfigPath); os.IsNotExist(err) {
		return nil, err
	}
	return ini.LoadSources(
		ini.LoadOptions{AllowBooleanKeys: true},
		C.ConfigPath,
	)
}

// Parse config
func (c *Config) Parse() error {
	cfg, err := c.readConfig()
	if err != nil {
		return err
	}

	if err := c.parseGeneral(cfg); err != nil {
		return err
	}

	if err := c.parseProxies(cfg); err != nil {
		return err
	}

	return c.parseRules(cfg)
}

// Proxies return proxies of clash
func (c *Config) Proxies() map[string]C.Proxy {
	return c.proxies
}

// Rules return rules of clash
func (c *Config) Rules() []C.Rule {
	return c.rules
}

// SetMode change mode of clash
func (c *Config) SetMode(mode Mode) {
	c.general.Mode = mode
	c.event <- &Event{Type: "mode", Payload: mode}
}

// SetLogLevel change log level of clash
func (c *Config) SetLogLevel(level C.LogLevel) {
	c.general.LogLevel = level
	c.event <- &Event{Type: "log-level", Payload: level}
}

// General return clash general config
func (c *Config) General() General {
	return *c.general
}

// UpdateRules is a function for hot reload rules
func (c *Config) UpdateRules() error {
	cfg, err := c.readConfig()
	if err != nil {
		return err
	}

	return c.parseRules(cfg)
}

func (c *Config) parseGeneral(cfg *ini.File) error {
	general := cfg.Section("General")

	port := general.Key("port").RangeInt(0, 1, 65535)
	socksPort := general.Key("socks-port").RangeInt(0, 1, 65535)
	redirPort := general.Key("redir-port").RangeInt(0, 1, 65535)
	allowLan := general.Key("allow-lan").MustBool()
	logLevelString := general.Key("log-level").MustString(C.INFO.String())
	modeString := general.Key("mode").MustString(Rule.String())

	mode, exist := ModeMapping[modeString]
	if !exist {
		return fmt.Errorf("General.mode value invalid")
	}

	logLevel, exist := C.LogLevelMapping[logLevelString]
	if !exist {
		return fmt.Errorf("General.log-level value invalid")
	}

	c.general = &General{
		Port:      port,
		SocksPort: socksPort,
		RedirPort: redirPort,
		AllowLan:  allowLan,
		Mode:      mode,
		LogLevel:  logLevel,
	}

	if restAddr := general.Key("external-controller").String(); restAddr != "" {
		c.event <- &Event{Type: "external-controller", Payload: restAddr}
	}

	c.UpdateGeneral(*c.general)
	return nil
}

// UpdateGeneral dispatch update event
func (c *Config) UpdateGeneral(general General) {
	c.UpdateProxy(ProxyConfig{
		Port:      &general.Port,
		SocksPort: &general.SocksPort,
		RedirPort: &general.RedirPort,
		AllowLan:  &general.AllowLan,
	})
	c.event <- &Event{Type: "mode", Payload: general.Mode}
	c.event <- &Event{Type: "log-level", Payload: general.LogLevel}
}

// UpdateProxy dispatch update proxy event
func (c *Config) UpdateProxy(pc ProxyConfig) {
	if pc.AllowLan != nil {
		c.general.AllowLan = *pc.AllowLan
	}

	if (pc.AllowLan != nil || pc.Port != nil) && *pc.Port != 0 {
		c.general.Port = *pc.Port
		c.event <- &Event{Type: "http-addr", Payload: genAddr(*pc.Port, c.general.AllowLan)}
	}

	if (pc.AllowLan != nil || pc.SocksPort != nil) && *pc.SocksPort != 0 {
		c.general.SocksPort = *pc.SocksPort
		c.event <- &Event{Type: "socks-addr", Payload: genAddr(*pc.SocksPort, c.general.AllowLan)}
	}

	if (pc.AllowLan != nil || pc.RedirPort != nil) && *pc.RedirPort != 0 {
		c.general.RedirPort = *pc.RedirPort
		c.event <- &Event{Type: "redir-addr", Payload: genAddr(*pc.RedirPort, c.general.AllowLan)}
	}
}

func (c *Config) parseProxies(cfg *ini.File) error {
	proxies := make(map[string]C.Proxy)
	proxiesConfig := cfg.Section("Proxy")
	groupsConfig := cfg.Section("Proxy Group")

	// parse proxy
	for _, key := range proxiesConfig.Keys() {
		proxy := key.Strings(",")
		if len(proxy) == 0 {
			continue
		}
		switch proxy[0] {
		// ss, server, port, cipter, password
		case "ss":
			if len(proxy) < 5 {
				continue
			}
			ssURL := fmt.Sprintf("ss://%s:%s@%s:%s", proxy[3], proxy[4], proxy[1], proxy[2])
			ss, err := adapters.NewShadowSocks(key.Name(), ssURL)
			if err != nil {
				return err
			}
			proxies[key.Name()] = ss
		// socks5, server, port
		case "socks5":
			if len(proxy) < 3 {
				continue
			}
			addr := fmt.Sprintf("%s:%s", proxy[1], proxy[2])
			socks5 := adapters.NewSocks5(key.Name(), addr)
			proxies[key.Name()] = socks5
		}
	}

	// parse proxy group
	for _, key := range groupsConfig.Keys() {
		rule := strings.Split(key.Value(), ",")
		rule = trimArr(rule)
		switch rule[0] {
		case "url-test":
			if len(rule) < 4 {
				return fmt.Errorf("URLTest need more than 4 param")
			}
			proxyNames := rule[1 : len(rule)-2]
			delay, _ := strconv.Atoi(rule[len(rule)-1])
			url := rule[len(rule)-2]
			var ps []C.Proxy
			for _, name := range proxyNames {
				if p, ok := proxies[name]; ok {
					ps = append(ps, p)
				}
			}

			adapter, err := adapters.NewURLTest(key.Name(), ps, url, time.Duration(delay)*time.Second)
			if err != nil {
				return fmt.Errorf("Config error: %s", err.Error())
			}
			proxies[key.Name()] = adapter
		case "select":
			if len(rule) < 2 {
				return fmt.Errorf("Selector need more than 2 param")
			}
			proxyNames := rule[1:]
			selectProxy := make(map[string]C.Proxy)
			for _, name := range proxyNames {
				proxy, exist := proxies[name]
				if !exist {
					return fmt.Errorf("Proxy %s not exist", name)
				}
				selectProxy[name] = proxy
			}
			selector, err := adapters.NewSelector(key.Name(), selectProxy)
			if err != nil {
				return fmt.Errorf("Selector create error: %s", err.Error())
			}
			proxies[key.Name()] = selector
		}
	}

	// init proxy
	proxies["GLOBAL"], _ = adapters.NewSelector("GLOBAL", proxies)
	proxies["DIRECT"] = adapters.NewDirect()
	proxies["REJECT"] = adapters.NewReject()

	c.proxies = proxies
	c.event <- &Event{Type: "proxies", Payload: proxies}
	return nil
}

func (c *Config) parseRules(cfg *ini.File) error {
	rules := []C.Rule{}

	rulesConfig := cfg.Section("Rule")
	// parse rules
	for _, key := range rulesConfig.Keys() {
		rule := strings.Split(key.Name(), ",")
		if len(rule) < 3 {
			continue
		}
		rule = trimArr(rule)
		switch rule[0] {
		case "DOMAIN-SUFFIX":
			rules = append(rules, R.NewDomainSuffix(rule[1], rule[2]))
		case "DOMAIN-KEYWORD":
			rules = append(rules, R.NewDomainKeyword(rule[1], rule[2]))
		case "GEOIP":
			rules = append(rules, R.NewGEOIP(rule[1], rule[2]))
		case "IP-CIDR", "IP-CIDR6":
			rules = append(rules, R.NewIPCIDR(rule[1], rule[2]))
		case "FINAL":
			rules = append(rules, R.NewFinal(rule[2]))
		}
	}

	c.rules = rules
	c.event <- &Event{Type: "rules", Payload: rules}
	return nil
}

func (c *Config) handleResponseMessage() {
	for elm := range c.reportCh {
		event := elm.(*Event)
		switch event.Type {
		case "http-addr":
			if event.Payload.(bool) == false {
				log.Errorf("Listening HTTP proxy at %s error", c.general.Port)
				c.general.Port = 0
			}
		case "socks-addr":
			if event.Payload.(bool) == false {
				log.Errorf("Listening SOCKS proxy at %s error", c.general.SocksPort)
				c.general.SocksPort = 0
			}
		case "redir-addr":
			if event.Payload.(bool) == false {
				log.Errorf("Listening Redir proxy at %s error", c.general.RedirPort)
				c.general.RedirPort = 0
			}
		}
	}
}

func newConfig() *Config {
	event := make(chan interface{})
	reportCh := make(chan interface{})
	config := &Config{
		general:    &General{},
		proxies:    make(map[string]C.Proxy),
		rules:      []C.Rule{},
		lastUpdate: time.Now(),

		event:      event,
		reportCh:   reportCh,
		observable: observable.NewObservable(event),
	}
	go config.handleResponseMessage()
	return config
}

// Instance return singleton instance of Config
func Instance() *Config {
	once.Do(func() {
		config = newConfig()
	})
	return config
}
