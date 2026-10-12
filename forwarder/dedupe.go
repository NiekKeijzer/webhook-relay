package forwarder

import "time"

// dedupeKey identifies one delivery of one request to one target.
type dedupeKey struct {
	route, target, deliveryID string
}

// dedupe remembers delivered keys for a window, in memory and bounded in
// size. It is best effort and lost on restart (§9.1). It is not safe for
// concurrent use; the forwarder processes one message at a time.
type dedupe struct {
	window time.Duration
	max    int
	at     map[dedupeKey]time.Time
	order  []dedupeEntry // oldest first
}

type dedupeEntry struct {
	key dedupeKey
	at  time.Time
}

func newDedupe(window time.Duration, max int) *dedupe {
	return &dedupe{window: window, max: max, at: map[dedupeKey]time.Time{}}
}

// seen reports whether k was added within the window before now.
func (d *dedupe) seen(k dedupeKey, now time.Time) bool {
	d.expire(now)
	_, ok := d.at[k]
	return ok
}

// add records k as delivered at now. When the store is full, the oldest key
// is dropped.
func (d *dedupe) add(k dedupeKey, now time.Time) {
	d.expire(now)
	d.at[k] = now
	d.order = append(d.order, dedupeEntry{key: k, at: now})
	for len(d.at) > d.max {
		d.dropOldest()
	}
}

func (d *dedupe) expire(now time.Time) {
	for len(d.order) > 0 && now.Sub(d.order[0].at) >= d.window {
		d.dropOldest()
	}
}

func (d *dedupe) dropOldest() {
	e := d.order[0]
	d.order = d.order[1:]
	// The key may have been added again later; then that entry stays.
	if at, ok := d.at[e.key]; ok && at.Equal(e.at) {
		delete(d.at, e.key)
	}
}
