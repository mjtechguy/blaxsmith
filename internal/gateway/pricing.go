package gateway

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Rates are USD micros per million tokens ($15/MTok = 15_000_000).
type Rates struct {
	Input, Output, CacheRead, CacheWrite int64
}

// Price is the rate card applied to one request and where it came from.
type Price struct {
	Rates   Rates
	Version string // "manifest:<date>", "override:<id>", or "" when unpriced
}

// Cost is the estimated cost in USD micros, rounded half up. It is always
// presented as an estimate: list or contracted rates, not the provider bill.
func Cost(u Usage, r Rates) int64 {
	// Integer math: exact up to ~10^11 tokens per class at $100/MTok.
	total := u.Input*r.Input + u.Output*r.Output + u.CacheRead*r.CacheRead + u.CacheWrite*r.CacheWrite
	return (total + 500_000) / 1_000_000
}

func manifestRates(p access.ModelPrice) Rates {
	micros := func(usd float64) int64 { return int64(math.Round(usd * 1e6)) }
	return Rates{micros(p.Input), micros(p.Output), micros(p.CacheRead), micros(p.CacheWrite)}
}

// ManifestPrice is the bundled list price. OpenCode Zen and Go resell
// provider models under their own ids; their rates are not bundled.
func ManifestPrice(provider, model string) Price {
	price, version, ok := access.ManifestPrice(provider, model)
	if !ok {
		return Price{}
	}
	return Price{Rates: manifestRates(price), Version: version}
}

// PriceBook resolves an organization's effective price: the newest admin
// override in effect, else the bundled manifest price.
type PriceBook struct {
	DB  *pgxpool.Pool
	TTL time.Duration // override cache; zero means 30 s

	mu    sync.Mutex
	cache map[string]cachedPrice
}

type cachedPrice struct {
	price Price
	until time.Time
}

func (b *PriceBook) Lookup(ctx context.Context, orgID, provider, model string, at time.Time) Price {
	ctx = tenant.Org(ctx, orgID)
	if b == nil || b.DB == nil {
		return ManifestPrice(provider, model)
	}
	key := strings.Join([]string{orgID, provider, model}, "\x00")
	now := time.Now()
	b.mu.Lock()
	if hit, ok := b.cache[key]; ok && now.Before(hit.until) {
		b.mu.Unlock()
		return hit.price
	}
	b.mu.Unlock()
	price := ManifestPrice(provider, model)
	var id string
	var r Rates
	err := b.DB.QueryRow(ctx, `SELECT id,input_micros_per_mtok,output_micros_per_mtok,cache_read_micros_per_mtok,
		cache_write_micros_per_mtok FROM gateway_price_overrides
		WHERE organization_id=$1 AND provider=$2 AND model=$3 AND effective_from<=$4
		ORDER BY effective_from DESC LIMIT 1`, orgID, provider, model, at).Scan(&id, &r.Input, &r.Output, &r.CacheRead, &r.CacheWrite)
	if err == nil {
		price = Price{Rates: r, Version: "override:" + id}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return price // a transient read error falls back to list price; not cached.
	}
	ttl := b.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	b.mu.Lock()
	if b.cache == nil || len(b.cache) > 10000 {
		b.cache = map[string]cachedPrice{}
	}
	b.cache[key] = cachedPrice{price, now.Add(ttl)}
	b.mu.Unlock()
	return price
}
