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
)

type DoHConfig struct {
	Enabled   bool
	Endpoints []string
	Timeout   time.Duration
	CacheSize int
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
}

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
	http     *req.Client
	doh      DoHConfig
	dohCache *lru.Cache[string, dohCacheEntry]
	mu       sync.Mutex
	geoCache map[string]geoCacheEntry
	status   Status
}

func New(apiURL string, doh DoHConfig) *Inspector {
	if doh.Timeout <= 0 {
		doh.Timeout = 4 * time.Second
	}
	if doh.CacheSize <= 0 {
		doh.CacheSize = 2048
	}
	cache, _ := lru.New[string, dohCacheEntry](doh.CacheSize)
	httpClient := req.C().
		SetTimeout(5*time.Second).
		SetCommonRetryCount(2).
		SetCommonRetryBackoffInterval(250*time.Millisecond, 1500*time.Millisecond)
	return &Inspector{
		apiURL: apiURL, resolver: net.DefaultResolver, http: httpClient, doh: doh,
		dohCache: cache, geoCache: map[string]geoCacheEntry{}, status: Status{Enabled: doh.Enabled},
	}
}

func (i *Inspector) Inspect(ctx context.Context, name string) Report {
	r := Report{Registrable: domain.Registrable(name)}
	lookupCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	addrs, err := i.resolver.LookupIPAddr(lookupCtx, name)
	cancel()
	if err != nil {
		r.DNSError = err.Error()
	} else {
		i.classifyLocal(&r, addrs)
	}

	if len(r.A)+len(r.AAAA) > 0 {
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

	i.inspectGeoIP(ctx, &r)
	return r
}

func (i *Inspector) inspectAddresses(ctx context.Context, name string, addrs []net.IPAddr) Report {
	r := Report{Registrable: domain.Registrable(name)}
	i.classifyLocal(&r, addrs)
	if len(r.A)+len(r.AAAA) > 0 {
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
