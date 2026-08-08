package lookup

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"clashrulepilot/internal/domain"
	"github.com/jwwsjlm/req/v3"
)

type Report struct {
	A, AAAA      []string
	Registrable  string
	China        bool
	ChinaChecked int
	DNSError     string
	GeoError     string
	FakeIP       []string
}

type geoCacheEntry struct {
	china   bool
	expires time.Time
}

type Inspector struct {
	apiURL   string
	resolver *net.Resolver
	http     *req.Client
	mu       sync.Mutex
	cache    map[string]geoCacheEntry
}

func New(apiURL string) *Inspector {
	httpClient := req.C().
		SetTimeout(5*time.Second).
		SetCommonRetryCount(3).
		SetCommonRetryBackoffInterval(300*time.Millisecond, 2*time.Second)
	return &Inspector{
		apiURL: apiURL, resolver: net.DefaultResolver,
		http: httpClient, cache: map[string]geoCacheEntry{},
	}
}

func (i *Inspector) Inspect(ctx context.Context, name string) Report {
	lookupCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	addrs, err := i.resolver.LookupIPAddr(lookupCtx, name)
	if err != nil {
		return Report{Registrable: domain.Registrable(name), DNSError: err.Error()}
	}
	return i.inspectAddresses(ctx, name, addrs)
}

func (i *Inspector) inspectAddresses(ctx context.Context, name string, addrs []net.IPAddr) Report {
	r := Report{Registrable: domain.Registrable(name)}
	var ips []string
	seen := map[string]bool{}
	for _, addr := range addrs {
		ip := addr.IP.String()
		if seen[ip] {
			continue
		}
		seen[ip] = true
		if addr.IP.To4() != nil {
			r.A = append(r.A, ip)
		} else {
			r.AAAA = append(r.AAAA, ip)
		}
		if isFakeIP(ip) {
			r.FakeIP = append(r.FakeIP, ip)
			continue
		}
		ips = append(ips, ip)
	}
	if len(ips) > 8 {
		ips = ips[:8]
	}
	if i.apiURL == "" || len(ips) == 0 {
		return r
	}
	type result struct {
		china bool
		err   error
	}
	ch := make(chan result, len(ips))
	for _, ip := range ips {
		go func(ip string) {
			china, err := i.isChina(ctx, ip)
			ch <- result{china: china, err: err}
		}(ip)
	}
	var errors []string
	for range ips {
		one := <-ch
		if one.err != nil {
			errors = append(errors, one.err.Error())
			continue
		}
		r.ChinaChecked++
		if one.china {
			r.China = true
		}
	}
	if len(errors) > 0 && r.ChinaChecked == 0 {
		r.GeoError = errors[0]
	}
	return r
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
	now := time.Now()
	i.mu.Lock()
	if cached, ok := i.cache[ip]; ok && now.Before(cached.expires) {
		i.mu.Unlock()
		return cached.china, nil
	}
	i.mu.Unlock()
	endpoint := strings.ReplaceAll(i.apiURL, "{ip}", url.PathEscape(ip))
	var data map[string]any
	resp, err := i.http.R().
		SetContext(ctx).
		SetHeader("User-Agent", "ClashRulePilot/1.0").
		SetSuccessResult(&data).
		Get(endpoint)
	if err != nil {
		return false, err
	}
	if !resp.IsSuccessState() {
		return false, fmt.Errorf("GeoIP HTTP %d: %s", resp.StatusCode, strings.TrimSpace(resp.String()))
	}
	if success, ok := data["success"].(bool); ok && !success {
		return false, fmt.Errorf("GeoIP lookup rejected")
	}
	code := firstString(data, "country_code", "countryCode", "country_code2")
	if code == "" {
		return false, fmt.Errorf("GeoIP response has no country code")
	}
	china := strings.EqualFold(code, "CN")
	i.mu.Lock()
	i.cache[ip] = geoCacheEntry{china: china, expires: now.Add(24 * time.Hour)}
	i.mu.Unlock()
	return china, nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
