package lookup

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"clashrulepilot/internal/domain"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/jwwsjlm/req/v3"
	"golang.org/x/sync/singleflight"
)

type DoHConfig struct {
	// Enabled/Endpoints are retained for backwards compatibility with the
	// original single DoH configuration and older tests.
	Enabled      bool
	Endpoints    []string
	Domestic     DNSGroupConfig
	Foreign      DNSGroupConfig
	Timeout      time.Duration
	CacheSize    int
	QueryTimeout time.Duration
}

type DNSGroupConfig struct {
	Enabled   bool
	Endpoints []string
}

type DNSGroupResult struct {
	Group      string
	Provider   string
	Endpoint   string
	UsedBackup bool
	A          []string
	AAAA       []string
	CacheHit   bool
	Error      string
	Duration   time.Duration
}

type Report struct {
	LocalA, LocalAAAA []string
	A, AAAA           []string
	Registrable       string
	China             bool
	ChinaChecked      int
	DNSError          string
	GeoError          string
	FakeIP            []string
	DNSSource         string
	DoHProvider       string
	DoHError          string
	DoHCacheHit       bool
	GeoIPs            []GeoIPInfo
	Domestic          DNSGroupResult
	Foreign           DNSGroupResult
	Timing            Timing
	Shared            bool
}

type Timing struct{ Local, Domestic, Foreign, GeoIP, Total time.Duration }

// GeoIPInfo is the per-address result returned by the configured GeoIP API.
// Keeping the address alongside the response prevents the UI from showing an
// unexplained aggregate China=true/false result when a hostname has multiple
// CDN addresses in different regions.
type GeoIPInfo struct {
	IP          string
	Country     string
	CountryCode string
	Region      string
	City        string
	ISP         string
	Org         string
	ASN         string
	China       bool
	Error       string
}

type Status struct {
	Enabled      bool
	Domestic     bool
	Foreign      bool
	CacheEntries int
	LastProvider string
	LastError    string
}

type geoCacheEntry struct {
	info    GeoIPInfo
	expires time.Time
}

type dohCacheEntry struct {
	ips     []string
	err     string
	expires time.Time
}

type Inspector struct {
	apiURL   string
	resolver *net.Resolver
	lookupIP func(context.Context, string) ([]net.IPAddr, error)
	http     *req.Client
	doh      DoHConfig
	dohCache *lru.Cache[string, dohCacheEntry]
	mu       sync.Mutex
	geoCache map[string]geoCacheEntry
	status   Status
	legacy   bool
	group    singleflight.Group
}

func New(apiURL string, doh DoHConfig) *Inspector {
	legacy := len(doh.Domestic.Endpoints) == 0 && len(doh.Foreign.Endpoints) == 0 && len(doh.Endpoints) > 0
	if doh.Timeout <= 0 {
		doh.Timeout = 4 * time.Second
	}
	if doh.CacheSize <= 0 {
		doh.CacheSize = 2048
	}
	if doh.QueryTimeout <= 0 {
		doh.QueryTimeout = 15 * time.Second
	}
	// A caller using the legacy single-group fields keeps the old behaviour.
	// Production configuration supplies Domestic/Foreign explicitly.
	if len(doh.Domestic.Endpoints) == 0 && len(doh.Foreign.Endpoints) == 0 && len(doh.Endpoints) > 0 {
		doh.Foreign = DNSGroupConfig{Enabled: doh.Enabled, Endpoints: append([]string(nil), doh.Endpoints...)}
		doh.Domestic = DNSGroupConfig{}
	}
	cache, _ := lru.New[string, dohCacheEntry](doh.CacheSize)
	httpClient := req.C().
		SetTimeout(5*time.Second).
		SetCommonRetryCount(2).
		SetCommonRetryBackoffInterval(250*time.Millisecond, 1500*time.Millisecond)
	enabled := doh.Enabled || doh.Domestic.Enabled || doh.Foreign.Enabled
	inspector := &Inspector{
		apiURL: apiURL, resolver: net.DefaultResolver, http: httpClient, doh: doh,
		dohCache: cache, geoCache: map[string]geoCacheEntry{}, status: Status{Enabled: enabled, Domestic: doh.Domestic.Enabled, Foreign: doh.Foreign.Enabled}, legacy: legacy,
	}
	inspector.lookupIP = inspector.resolver.LookupIPAddr
	return inspector
}

func (i *Inspector) Inspect(ctx context.Context, name string) Report {
	key := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	ch := i.group.DoChan(key, func() (any, error) {
		jobCtx, cancel := context.WithTimeout(context.Background(), i.doh.QueryTimeout)
		defer cancel()
		return i.inspect(jobCtx, key), nil
	})
	select {
	case <-ctx.Done():
		return Report{Registrable: domain.Registrable(name), DNSError: ctx.Err().Error()}
	case result := <-ch:
		report := cloneReport(result.Val.(Report))
		report.Shared = result.Shared
		return report
	}
}

func (i *Inspector) inspect(ctx context.Context, name string) Report {
	started := time.Now()
	r := Report{Registrable: domain.Registrable(name)}
	localStarted := time.Now()
	lookupCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	resolver := i.lookupIP
	if resolver == nil {
		resolver = i.resolver.LookupIPAddr
	}
	addrs, err := resolver(lookupCtx, name)
	cancel()
	if err != nil {
		r.DNSError = err.Error()
	} else {
		i.classifyLocal(&r, addrs)
	}
	r.Timing.Local = time.Since(localStarted)

	if i.hasGroups() {
		r.Domestic, r.Foreign = i.resolveGroups(ctx, name)
		r.Timing.Domestic, r.Timing.Foreign = r.Domestic.Duration, r.Foreign.Duration
		r.A, r.AAAA = nil, nil
		i.mergeGroupAddresses(&r)
		if len(r.A)+len(r.AAAA) > 0 {
			r.DNSSource = "dual"
		}
	} else if len(r.A)+len(r.AAAA) > 0 {
		r.DNSSource = "local"
	} else if i.doh.Enabled {
		result, provider, cacheHit, dohErr := i.resolveDoH(ctx, name)
		if dohErr != nil {
			r.DoHError = dohErr.Error()
			i.setDoHStatus(provider, dohErr)
		} else {
			r.DNSSource = "doh"
			r.DoHProvider = provider
			r.DoHCacheHit = cacheHit
			for _, ip := range result {
				addr, parseErr := netip.ParseAddr(ip)
				if parseErr != nil || isFakeIP(ip) {
					continue
				}
				if addr.Is4() {
					r.A = appendUnique(r.A, ip)
				} else {
					r.AAAA = appendUnique(r.AAAA, ip)
				}
			}
			i.setDoHStatus(provider, nil)
		}
	}

	geoStarted := time.Now()
	i.inspectGeoIP(ctx, &r)
	r.Timing.GeoIP = time.Since(geoStarted)
	r.Timing.Total = time.Since(started)
	return r
}

func (i *Inspector) inspectAddresses(ctx context.Context, name string, addrs []net.IPAddr) Report {
	r := Report{Registrable: domain.Registrable(name)}
	i.classifyLocal(&r, addrs)
	if i.hasGroups() {
		r.Domestic, r.Foreign = i.resolveGroups(ctx, name)
		r.A, r.AAAA = nil, nil
		i.mergeGroupAddresses(&r)
		if len(r.A)+len(r.AAAA) > 0 {
			r.DNSSource = "dual"
		}
	} else if len(r.A)+len(r.AAAA) > 0 {
		r.DNSSource = "local"
	} else if i.doh.Enabled {
		result, provider, cacheHit, err := i.resolveDoH(ctx, name)
		if err != nil {
			r.DoHError = err.Error()
			i.setDoHStatus(provider, err)
		} else {
			r.DNSSource, r.DoHProvider, r.DoHCacheHit = "doh", provider, cacheHit
			for _, ip := range result {
				addr, parseErr := netip.ParseAddr(ip)
				if parseErr != nil || isFakeIP(ip) {
					continue
				}
				if addr.Is4() {
					r.A = appendUnique(r.A, ip)
				} else {
					r.AAAA = appendUnique(r.AAAA, ip)
				}
			}
			i.setDoHStatus(provider, nil)
		}
	}
	i.inspectGeoIP(ctx, &r)
	return r
}

func (i *Inspector) hasGroups() bool {
	return !i.legacy && ((i.doh.Domestic.Enabled && len(i.doh.Domestic.Endpoints) > 0) || (i.doh.Foreign.Enabled && len(i.doh.Foreign.Endpoints) > 0))
}

func (i *Inspector) resolveGroups(ctx context.Context, name string) (DNSGroupResult, DNSGroupResult) {
	type one struct {
		result DNSGroupResult
	}
	ch := make(chan one, 2)
	go func() { ch <- one{result: i.resolveGroup(ctx, name, "domestic", i.doh.Domestic)} }()
	go func() { ch <- one{result: i.resolveGroup(ctx, name, "foreign", i.doh.Foreign)} }()
	var domestic, foreign DNSGroupResult
	for range 2 {
		result := (<-ch).result
		if result.Group == "domestic" {
			domestic = result
		} else {
			foreign = result
		}
	}
	return domestic, foreign
}

func (i *Inspector) resolveGroup(ctx context.Context, name, group string, cfg DNSGroupConfig) DNSGroupResult {
	started := time.Now()
	result := DNSGroupResult{Group: group}
	defer func() { result.Duration = time.Since(started) }()
	if !cfg.Enabled || len(cfg.Endpoints) == 0 {
		result.Error = "未启用"
		return result
	}
	var errors []string
	for index, endpoint := range cfg.Endpoints {
		ch := make(chan struct {
			ips []string
			hit bool
			err error
		}, 2)
		for _, recordType := range []string{"A", "AAAA"} {
			go func(recordType string) {
				ips, hit, err := i.queryDoH(ctx, endpoint, name, recordType)
				ch <- struct {
					ips []string
					hit bool
					err error
				}{ips: ips, hit: hit, err: err}
			}(recordType)
		}
		var ips []string
		cacheHit := true
		for range 2 {
			one := <-ch
			cacheHit = cacheHit && one.hit
			if one.err != nil {
				errors = append(errors, providerName(endpoint)+" "+one.err.Error())
				continue
			}
			for _, ip := range one.ips {
				if !isFakeIP(ip) {
					ips = appendUnique(ips, ip)
				}
			}
		}
		if len(ips) == 0 {
			continue
		}
		result.Provider = providerName(endpoint)
		result.Endpoint = endpoint
		result.UsedBackup = index > 0
		result.CacheHit = cacheHit
		for _, ip := range ips {
			addr, err := netip.ParseAddr(ip)
			if err != nil {
				continue
			}
			if addr.Is4() {
				result.A = appendUnique(result.A, ip)
			} else {
				result.AAAA = appendUnique(result.AAAA, ip)
			}
		}
		if index > 0 && len(errors) > 0 {
			result.Error = "主端点失败，已切换备用：" + compact(strings.Join(errors, "；"))
		}
		i.setDoHStatus(result.Provider, nil)
		return result
	}
	if len(errors) > 0 {
		result.Error = compact(strings.Join(errors, "；"))
		i.setDoHStatus(providerNameForGroup(group), fmt.Errorf("%s", result.Error))
	} else {
		result.Error = "主备端点均未返回有效 A/AAAA"
	}
	return result
}

func cloneReport(in Report) Report {
	out := in
	out.LocalA = append([]string(nil), in.LocalA...)
	out.LocalAAAA = append([]string(nil), in.LocalAAAA...)
	out.A = append([]string(nil), in.A...)
	out.AAAA = append([]string(nil), in.AAAA...)
	out.FakeIP = append([]string(nil), in.FakeIP...)
	out.GeoIPs = append([]GeoIPInfo(nil), in.GeoIPs...)
	out.Domestic.A = append([]string(nil), in.Domestic.A...)
	out.Domestic.AAAA = append([]string(nil), in.Domestic.AAAA...)
	out.Foreign.A = append([]string(nil), in.Foreign.A...)
	out.Foreign.AAAA = append([]string(nil), in.Foreign.AAAA...)
	return out
}

func providerNameForGroup(group string) string {
	if group == "domestic" {
		return "国内 DNS"
	}
	return "国外 DNS"
}

func (i *Inspector) mergeGroupAddresses(r *Report) {
	for _, group := range []DNSGroupResult{r.Domestic, r.Foreign} {
		for _, ip := range group.A {
			r.A = appendUnique(r.A, ip)
		}
		for _, ip := range group.AAAA {
			r.AAAA = appendUnique(r.AAAA, ip)
		}
	}
	sort.Strings(r.A)
	sort.Strings(r.AAAA)
}

func (i *Inspector) classifyLocal(r *Report, addrs []net.IPAddr) {
	seen := map[string]bool{}
	for _, item := range addrs {
		ip := item.IP.String()
		if seen[ip] {
			continue
		}
		seen[ip] = true
		if item.IP.To4() != nil {
			r.LocalA = append(r.LocalA, ip)
		} else {
			r.LocalAAAA = append(r.LocalAAAA, ip)
		}
		if isFakeIP(ip) {
			r.FakeIP = append(r.FakeIP, ip)
			continue
		}
		if item.IP.To4() != nil {
			r.A = append(r.A, ip)
		} else {
			r.AAAA = append(r.AAAA, ip)
		}
	}
	sort.Strings(r.LocalA)
	sort.Strings(r.LocalAAAA)
	sort.Strings(r.A)
	sort.Strings(r.AAAA)
	sort.Strings(r.FakeIP)
}

func (i *Inspector) inspectGeoIP(ctx context.Context, r *Report) {
	ips := append(append([]string{}, r.A...), r.AAAA...)
	if len(ips) > 8 {
		ips = ips[:8]
	}
	if i.apiURL == "" || len(ips) == 0 {
		return
	}
	type result struct {
		index int
		info  GeoIPInfo
		err   error
	}
	ch := make(chan result, len(ips))
	for index, ip := range ips {
		go func(index int, ip string) {
			info, err := i.lookupGeo(ctx, ip)
			ch <- result{index: index, info: info, err: err}
		}(index, ip)
	}
	var errors []string
	results := make([]GeoIPInfo, len(ips))
	for range ips {
		one := <-ch
		if one.err != nil {
			one.info.Error = compact(one.err.Error())
		}
		results[one.index] = one.info
		if one.err != nil {
			errors = append(errors, one.err.Error())
			continue
		}
		r.ChinaChecked++
		if one.info.China {
			r.China = true
		}
	}
	for _, info := range results {
		if info.IP != "" {
			r.GeoIPs = append(r.GeoIPs, info)
		}
	}
	if len(errors) > 0 && r.ChinaChecked == 0 {
		r.GeoError = errors[0]
	}
}

type dohResponse struct {
	Status int `json:"Status"`
	Answer []struct {
		Name string `json:"name"`
		Type int    `json:"type"`
		TTL  int    `json:"TTL"`
		Data string `json:"data"`
	} `json:"Answer"`
}

func (i *Inspector) resolveDoH(ctx context.Context, name string) ([]string, string, bool, error) {
	var errors []string
	for _, endpoint := range i.doh.Endpoints {
		provider := providerName(endpoint)
		type oneResult struct {
			ips      []string
			cacheHit bool
			err      error
		}
		ch := make(chan oneResult, 2)
		for _, recordType := range []string{"A", "AAAA"} {
			go func(recordType string) {
				ips, hit, err := i.queryDoH(ctx, endpoint, name, recordType)
				ch <- oneResult{ips: ips, cacheHit: hit, err: err}
			}(recordType)
		}
		var ips []string
		cacheHit := true
		for range 2 {
			one := <-ch
			cacheHit = cacheHit && one.cacheHit
			if one.err != nil {
				errors = append(errors, provider+": "+one.err.Error())
				continue
			}
			for _, ip := range one.ips {
				if !isFakeIP(ip) {
					ips = appendUnique(ips, ip)
				}
			}
		}
		if len(ips) > 0 {
			sort.Strings(ips)
			return ips, provider, cacheHit, nil
		}
	}
	if len(errors) == 0 {
		errors = append(errors, "所有 DoH 服务均未返回 A/AAAA")
	}
	return nil, "", false, fmt.Errorf("%s", strings.Join(errors, "；"))
}

func (i *Inspector) queryDoH(ctx context.Context, endpoint, name, recordType string) ([]string, bool, error) {
	key := endpoint + "\x00" + strings.ToLower(name) + "\x00" + recordType
	now := time.Now()
	if cached, ok := i.dohCache.Get(key); ok && now.Before(cached.expires) {
		if cached.err != "" {
			return nil, true, fmt.Errorf("%s", cached.err)
		}
		return append([]string(nil), cached.ips...), true, nil
	}

	u, err := url.Parse(endpoint)
	loopbackHTTP := u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback())
	if err != nil || u.Host == "" || (u.Scheme != "https" && !loopbackHTTP) {
		return nil, false, fmt.Errorf("无效 DoH URL")
	}
	q := u.Query()
	q.Set("name", name)
	q.Set("type", recordType)
	u.RawQuery = q.Encode()
	requestCtx, cancel := context.WithTimeout(ctx, i.doh.Timeout)
	defer cancel()
	var data dohResponse
	resp, err := i.http.R().
		SetContext(requestCtx).
		SetHeader("Accept", "application/dns-json").
		SetHeader("User-Agent", "ClashRulePilot/1.0").
		SetSuccessResult(&data).
		Get(u.String())
	if err != nil {
		i.dohCache.Add(key, dohCacheEntry{err: compact(err.Error()), expires: now.Add(30 * time.Second)})
		return nil, false, err
	}
	if !resp.IsSuccessState() {
		err = fmt.Errorf("HTTP %d", resp.StatusCode)
		i.dohCache.Add(key, dohCacheEntry{err: err.Error(), expires: now.Add(30 * time.Second)})
		return nil, false, err
	}
	if data.Status != 0 {
		err = fmt.Errorf("DNS status %d", data.Status)
		i.dohCache.Add(key, dohCacheEntry{err: err.Error(), expires: now.Add(30 * time.Second)})
		return nil, false, err
	}
	wantType := 1
	if recordType == "AAAA" {
		wantType = 28
	}
	minTTL := 3600
	var ips []string
	for _, answer := range data.Answer {
		if answer.Type != wantType {
			continue
		}
		addr, parseErr := netip.ParseAddr(strings.TrimSpace(answer.Data))
		if parseErr != nil || (wantType == 1 && !addr.Is4()) || (wantType == 28 && !addr.Is6()) {
			continue
		}
		ips = appendUnique(ips, addr.String())
		if answer.TTL > 0 && answer.TTL < minTTL {
			minTTL = answer.TTL
		}
	}
	if minTTL < 60 {
		minTTL = 60
	}
	if minTTL > 3600 {
		minTTL = 3600
	}
	expires := now.Add(time.Duration(minTTL) * time.Second)
	if len(ips) == 0 {
		expires = now.Add(30 * time.Second)
	}
	i.dohCache.Add(key, dohCacheEntry{ips: append([]string(nil), ips...), expires: expires})
	return ips, false, nil
}

var fakeIPPrefixes = []netip.Prefix{
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("fdfe:dcba:9876::/64"),
}

func isFakeIP(value string) bool {
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return false
	}
	for _, prefix := range fakeIPPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (i *Inspector) isChina(ctx context.Context, ip string) (bool, error) {
	info, err := i.lookupGeo(ctx, ip)
	return info.China, err
}

func (i *Inspector) lookupGeo(ctx context.Context, ip string) (GeoIPInfo, error) {
	now := time.Now()
	i.mu.Lock()
	if cached, ok := i.geoCache[ip]; ok && now.Before(cached.expires) {
		i.mu.Unlock()
		return cached.info, nil
	}
	i.mu.Unlock()
	info := GeoIPInfo{IP: ip}
	endpoint := strings.ReplaceAll(i.apiURL, "{ip}", url.PathEscape(ip))
	var data map[string]any
	resp, err := i.http.R().
		SetContext(ctx).
		SetHeader("User-Agent", "ClashRulePilot/1.0").
		SetSuccessResult(&data).
		Get(endpoint)
	if err != nil {
		return info, err
	}
	if !resp.IsSuccessState() {
		return info, fmt.Errorf("GeoIP HTTP %d: %s", resp.StatusCode, strings.TrimSpace(resp.String()))
	}
	if success, ok := data["success"].(bool); ok && !success {
		return info, fmt.Errorf("GeoIP lookup rejected")
	}
	code := firstString(data, "country_code", "countryCode", "country_code2")
	if code == "" {
		return info, fmt.Errorf("GeoIP response has no country code")
	}
	info.CountryCode = strings.ToUpper(code)
	info.Country = firstString(data, "country")
	info.Region = firstString(data, "region", "region_name")
	info.City = firstString(data, "city")
	if connection, ok := data["connection"].(map[string]any); ok {
		info.ISP = firstString(connection, "isp")
		info.Org = firstString(connection, "org", "organization")
		info.ASN = firstString(connection, "asn")
	}
	if info.ASN == "" {
		info.ASN = firstString(data, "asn")
	}
	info.China = strings.EqualFold(info.CountryCode, "CN")
	i.mu.Lock()
	i.geoCache[ip] = geoCacheEntry{info: info, expires: now.Add(24 * time.Hour)}
	i.mu.Unlock()
	return info, nil
}

func (i *Inspector) Status() Status {
	i.mu.Lock()
	defer i.mu.Unlock()
	status := i.status
	status.CacheEntries = i.dohCache.Len()
	return status
}

func (i *Inspector) setDoHStatus(provider string, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if provider != "" {
		i.status.LastProvider = provider
	}
	if err == nil {
		i.status.LastError = ""
	} else {
		i.status.LastError = compact(err.Error())
	}
}

func providerName(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case strings.Contains(host, "cloudflare"):
		return "Cloudflare"
	case strings.Contains(host, "google"):
		return "Google"
	default:
		return host
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func compact(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 240 {
		return value[:240] + "…"
	}
	return value
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := valueString(m[key]); value != "" {
			return value
		}
	}
	return ""
}

func valueString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%g", v)
	case float32:
		return fmt.Sprintf("%g", v)
	case int:
		return fmt.Sprintf("%d", v)
	case int64:
		return fmt.Sprintf("%d", v)
	default:
		return ""
	}
}
