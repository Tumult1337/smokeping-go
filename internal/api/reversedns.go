package api

import (
	"container/list"
	"context"
	"net/netip"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	reverseDNSMaxEntries           = 4096
	reverseDNSMaxConcurrent        = 8
	reverseDNSMaxMissesPerResponse = 128
	reverseDNSResponseTimeout      = 2 * time.Second
	reverseDNSPositiveTTL          = 24 * time.Hour
	reverseDNSNegativeTTL          = 5 * time.Minute
)

type reverseLookupFunc func(context.Context, string) ([]string, error)

type reverseDNSEntry struct {
	addr    netip.Addr
	name    string
	expires time.Time
}

type reverseDNSCall struct {
	done chan struct{}
	name string
}

type reverseDNSCache struct {
	lookup          reverseLookupFunc
	now             func() time.Time
	limit           chan struct{}
	responseTimeout time.Duration

	mu         sync.Mutex
	entries    map[netip.Addr]*list.Element
	order      *list.List
	inflight   map[netip.Addr]*reverseDNSCall
	maxEntries int
}

type reverseDNSGroup struct {
	addr netip.Addr
	raw  []string
}

func newReverseDNSCache(lookup reverseLookupFunc) *reverseDNSCache {
	return &reverseDNSCache{
		lookup:          lookup,
		now:             time.Now,
		limit:           make(chan struct{}, reverseDNSMaxConcurrent),
		responseTimeout: reverseDNSResponseTimeout,
		entries:         make(map[netip.Addr]*list.Element, reverseDNSMaxEntries),
		order:           list.New(),
		inflight:        make(map[netip.Addr]*reverseDNSCall),
		maxEntries:      reverseDNSMaxEntries,
	}
}

func (c *reverseDNSCache) hostnames(ctx context.Context, ips []string) map[string]string {
	names := make(map[string]string)
	groups := groupReverseDNSAddrs(ips)
	missing := make([]reverseDNSGroup, 0, min(len(groups), reverseDNSMaxMissesPerResponse))
	for _, group := range groups {
		name, ok := c.cached(group.addr)
		if ok {
			addReverseDNSName(names, group.raw, name)
			continue
		}
		if len(missing) < reverseDNSMaxMissesPerResponse {
			missing = append(missing, group)
		}
	}

	lookupCtx, cancel := context.WithTimeout(ctx, c.responseTimeout)
	defer cancel()
	if len(missing) == 0 {
		return names
	}

	type result struct {
		group reverseDNSGroup
		name  string
	}
	jobs := make(chan reverseDNSGroup, len(missing))
	results := make(chan result, len(missing))
	for _, group := range missing {
		jobs <- group
	}
	close(jobs)

	var workers sync.WaitGroup
	for range min(len(missing), reverseDNSMaxConcurrent) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for group := range jobs {
				results <- result{group: group, name: c.resolve(lookupCtx, group.addr)}
			}
		}()
	}
	workers.Wait()
	close(results)
	for result := range results {
		addReverseDNSName(names, result.group.raw, result.name)
	}
	return names
}

func groupReverseDNSAddrs(ips []string) []reverseDNSGroup {
	groups := make([]reverseDNSGroup, 0, min(len(ips), reverseDNSMaxMissesPerResponse))
	byAddr := make(map[netip.Addr]int, len(groups))
	seenRaw := make(map[string]bool, len(ips))
	for _, raw := range ips {
		addr, ok := canonicalHopAddr(raw)
		if !ok || seenRaw[raw] {
			continue
		}
		seenRaw[raw] = true
		if i, exists := byAddr[addr]; exists {
			groups[i].raw = append(groups[i].raw, raw)
			continue
		}
		byAddr[addr] = len(groups)
		groups = append(groups, reverseDNSGroup{addr: addr, raw: []string{raw}})
	}
	return groups
}

func canonicalHopAddr(raw string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, false
	}
	addr = addr.Unmap()
	if addr.Is6() {
		addr = addr.WithZone("")
	}
	return addr, true
}

func addReverseDNSName(dst map[string]string, raw []string, name string) {
	if name == "" {
		return
	}
	for _, ip := range raw {
		dst[ip] = name
	}
}

func (c *reverseDNSCache) cached(addr netip.Addr) (string, bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cachedLocked(addr, now)
}

func (c *reverseDNSCache) cachedLocked(addr netip.Addr, now time.Time) (string, bool) {
	elem, ok := c.entries[addr]
	if !ok {
		return "", false
	}
	entry := elem.Value.(*reverseDNSEntry)
	if !now.Before(entry.expires) {
		delete(c.entries, addr)
		c.order.Remove(elem)
		return "", false
	}
	c.order.MoveToFront(elem)
	return entry.name, true
}

func (c *reverseDNSCache) resolve(ctx context.Context, addr netip.Addr) string {
	if name, ok := c.cached(addr); ok {
		return name
	}

	c.mu.Lock()
	if call := c.inflight[addr]; call != nil {
		c.mu.Unlock()
		return waitReverseDNSCall(ctx, call)
	}
	c.mu.Unlock()

	select {
	case c.limit <- struct{}{}:
	case <-ctx.Done():
		return ""
	}

	if name, ok := c.cached(addr); ok {
		<-c.limit
		return name
	}
	c.mu.Lock()
	if call := c.inflight[addr]; call != nil {
		c.mu.Unlock()
		<-c.limit
		return waitReverseDNSCall(ctx, call)
	}
	call := &reverseDNSCall{done: make(chan struct{})}
	c.inflight[addr] = call
	c.mu.Unlock()

	resolved, err := c.lookup(ctx, addr.String())
	name := ""
	ttl := reverseDNSNegativeTTL
	if err == nil {
		name = usablePTRName(resolved)
		if name != "" {
			ttl = reverseDNSPositiveTTL
		}
	}
	c.mu.Lock()
	c.storeLocked(addr, name, c.now().Add(ttl))
	if c.inflight[addr] == call {
		call.name = name
		delete(c.inflight, addr)
		close(call.done)
	}
	c.mu.Unlock()
	<-c.limit
	return name
}

func waitReverseDNSCall(ctx context.Context, call *reverseDNSCall) string {
	select {
	case <-call.done:
		return call.name
	case <-ctx.Done():
		return ""
	}
}

func (c *reverseDNSCache) storeLocked(addr netip.Addr, name string, expires time.Time) {
	if elem, ok := c.entries[addr]; ok {
		entry := elem.Value.(*reverseDNSEntry)
		entry.name = name
		entry.expires = expires
		c.order.MoveToFront(elem)
	} else {
		elem := c.order.PushFront(&reverseDNSEntry{addr: addr, name: name, expires: expires})
		c.entries[addr] = elem
	}
	for len(c.entries) > c.maxEntries {
		oldest := c.order.Back()
		entry := oldest.Value.(*reverseDNSEntry)
		delete(c.entries, entry.addr)
		c.order.Remove(oldest)
	}
}

func usablePTRName(names []string) string {
	for _, candidate := range names {
		candidate = strings.TrimSuffix(candidate, ".")
		if candidate == "" || len(candidate) > 253 || !utf8.ValidString(candidate) ||
			strings.IndexFunc(candidate, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			continue
		}
		return candidate
	}
	return ""
}
