/*
 * Copyright 2019 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Ristretto is a fast, fixed size, in-memory cache with a dual focus on
// throughput and hit ratio performance. You can easily add Ristretto to an
// existing system and keep the most valuable data where you need it.
package ristretto

import (
	"bytes"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/dgraph-io/ristretto/z"
)

const (
	// TODO: find the optimal value for this or make it configurable
	setBufSize = 32 * 1024
)

// Cache is a thread-safe implementation of a hashmap with a TinyLFU admission
// policy and a Sampled LFU eviction policy. You can use the same Cache instance
// from as many goroutines as you want.
type Cache struct {
	// store is the central concurrent hashmap where key-value items are stored
	store store
	// policy determines what gets let in to the cache and what gets kicked out
	policy policy
	// getBuf is a custom ring buffer implementation that gets pushed to when
	// keys are read
	getBuf *ringBuffer
	// setBuf is a buffer allowing us to batch/drop Sets during times of high
	// contention
	setBuf chan *item
	// onEvict is called for item evictions
	onEvict func(uint64, interface{}, int64)
	// KeyToHash function is used to customize the key hashing algorithm.
	// Each key will be hashed using the provided function. If keyToHash value
	// is not set, the default keyToHash function is used.
	keyToHash func(interface{}, uint8) uint64
	// stop is used to stop the processItems goroutine
	stop chan struct{}
	// cost calculates cost from a value
	cost func(value interface{}) int64
	// Metrics contains a running log of important statistics like hits, misses,
	// and dropped items
	Metrics *Metrics
}

// Config is passed to NewCache for creating new Cache instances.
type Config struct {
	// NumCounters determines the number of counters (keys) to keep that hold
	// access frequency information. It's generally a good idea to have more
	// counters than the max cache capacity, as this will improve eviction
	// accuracy and subsequent hit ratios.
	//
	// For example, if you expect your cache to hold 1,000,000 items when full,
	// NumCounters should be 10,000,000 (10x). Each counter takes up 4 bits, so
	// keeping 10,000,000 counters would require 5MB of memory.
	NumCounters int64
	// MaxCost can be considered as the cache capacity, in whatever units you
	// choose to use.
	//
	// For example, if you want the cache to have a max capacity of 100MB, you
	// would set MaxCost to 100,000,000 and pass an item's number of bytes as
	// the `cost` parameter for calls to Set. If new items are accepted, the
	// eviction process will take care of making room for the new item and not
	// overflowing the MaxCost value.
	MaxCost int64
	// BufferItems determines the size of Get buffers.
	//
	// Unless you have a rare use case, using `64` as the BufferItems value
	// results in good performance.
	BufferItems int64
	// Metrics determines whether cache statistics are kept during the cache's
	// lifetime. There *is* some overhead to keeping statistics, so you should
	// only set this flag to true when testing or throughput performance isn't a
	// major factor.
	Metrics bool
	// OnEvict is called for every eviction and passes the hashed key, value,
	// and cost to the function.
	OnEvict func(key uint64, value interface{}, cost int64)
	// KeyToHash function is used to customize the key hashing algorithm.
	// Each key will be hashed using the provided function. If keyToHash value
	// is not set, the default keyToHash function is used.
	KeyToHash func(key interface{}, seed uint8) uint64
	// Cost evaluates a value and outputs a corresponding cost. This function
	// is ran after Set is called for a new item or an item update with a cost
	// param of 0.
	Cost func(value interface{}) int64
	// Hashes is the number of 64-bit hashes to chain and use as each item's
	// unique identifier. For example, setting Hashes to 2 will set internal
	// keys to 128-bits and therefore very little probability of colliding with
	// another key-value item in the cache. To just use 64-bit keys, set this
	// value to 0 or 1.
	//
	// The larger this value is, the worse throughput performance will be.
	Hashes uint8
}

type itemFlag byte

const (
	itemNew itemFlag = iota
	itemDelete
	itemUpdate
)

// item is passed to setBuf so items can eventually be added to the cache
type item struct {
	flag    itemFlag
	key     interface{}
	keyHash uint64
	value   interface{}
	cost    int64
}

// NewCache returns a new Cache instance and any configuration errors, if any.
func NewCache(config *Config) (*Cache, error) {
	switch {
	case config.NumCounters == 0:
		return nil, errors.New("NumCounters can't be zero.")
	case config.MaxCost == 0:
		return nil, errors.New("MaxCost can't be zero.")
	case config.BufferItems == 0:
		return nil, errors.New("BufferItems can't be zero.")
	}
	policy := newPolicy(config.NumCounters, config.MaxCost)
	cache := &Cache{
		store:     newStore(config.Hashes),
		policy:    policy,
		getBuf:    newRingBuffer(policy, config.BufferItems),
		setBuf:    make(chan *item, setBufSize),
		onEvict:   config.OnEvict,
		keyToHash: config.KeyToHash,
		stop:      make(chan struct{}),
		cost:      config.Cost,
	}
	if cache.keyToHash == nil {
		cache.keyToHash = z.KeyToHash
	}
	if config.Metrics {
		cache.collectMetrics()
	}
	// NOTE: benchmarks seem to show that performance decreases the more
	//       goroutines we have running cache.processItems(), so 1 should
	//       usually be sufficient
	go cache.processItems()
	return cache, nil
}

// Get returns the value (if any) and a boolean representing whether the
// value was found or not. The value can be nil and the boolean can be true at
// the same time.
func (c *Cache) Get(key interface{}) (interface{}, bool) {
	if c == nil || key == nil {
		return nil, false
	}
	hashed := z.KeyToHash(key, 0)
	c.getBuf.Push(hashed)
	value, ok := c.store.Get(hashed, key)
	if ok {
		c.Metrics.add(hit, hashed, 1)
	} else {
		c.Metrics.add(miss, hashed, 1)
	}
	return value, ok
}

// Set attempts to add the key-value item to the cache. If it returns false,
// then the Set was dropped and the key-value item isn't added to the cache. If
// it returns true, there's still a chance it could be dropped by the policy if
// its determined that the key-value item isn't worth keeping, but otherwise the
// item will be added and other items will be evicted in order to make room.
//
// To dynamically evaluate the items cost using the Config.Coster function, set
// the cost parameter to 0 and Coster will be ran when needed in order to find
// the items true cost.
func (c *Cache) Set(key, value interface{}, cost int64) bool {
	if c == nil || key == nil {
		return false
	}
	i := &item{
		flag:    itemNew,
		key:     key,
		keyHash: z.KeyToHash(key, 0),
		value:   value,
		cost:    cost,
	}
	// attempt to immediately update hashmap value and set flag to update so the
	// cost is eventually updated
	if c.store.Update(i.keyHash, i.key, i.value) {
		i.flag = itemUpdate
	}
	// attempt to send item to policy
	select {
	case c.setBuf <- i:
		return true
	default:
		c.Metrics.add(dropSets, i.keyHash, 1)
		return false
	}
}

// Del deletes the key-value item from the cache if it exists.
func (c *Cache) Del(key interface{}) {
	if c == nil || key == nil {
		return
	}
	c.setBuf <- &item{
		flag:    itemDelete,
		key:     key,
		keyHash: z.KeyToHash(key, 0),
	}
}

// Close stops all goroutines and closes all channels.
func (c *Cache) Close() {
	// block until processItems goroutine is returned
	c.stop <- struct{}{}
	close(c.stop)
	close(c.setBuf)
	c.policy.Close()
}

// Clear empties the hashmap and zeroes all policy counters. Note that this is
// not an atomic operation (but that shouldn't be a problem as it's assumed that
// Set/Get calls won't be occurring until after this).
func (c *Cache) Clear() {
	// block until processItems goroutine is returned
	c.stop <- struct{}{}
	// swap out the setBuf channel
	c.setBuf = make(chan *item, setBufSize)
	// clear value hashmap and policy data
	c.policy.Clear()
	c.store.Clear()
	// only reset metrics if they're enabled
	if c.Metrics != nil {
		c.Metrics.Clear()
	}
	// restart processItems goroutine
	go c.processItems()
}

// processItems is ran by goroutines processing the Set buffer.
func (c *Cache) processItems() {
	for {
		select {
		case i := <-c.setBuf:
			// calculate item cost value if new or update
			if i.cost == 0 && c.cost != nil && i.flag != itemDelete {
				i.cost = c.cost(i.value)
			}
			switch i.flag {
			case itemNew:
				victims, added := c.policy.Add(i.keyHash, i.cost);
				if added {
					// item was accepted by the policy, so add to the hashmap
					c.store.Set(i.keyHash, i.key, i.value)
				}
				// delete victims
				for _, victim := range victims {
					// TODO: make Get-Delete atomic
					if c.onEvict != nil {
						// force get with no collision checking because
						// we don't have access to the victim's key
						victim.value, _ = c.store.Get(victim.keyHash, nil)
						c.onEvict(victim.keyHash, victim.value, victim.cost)
					}
					// force delete with no collision checking because we
					// don't have access to the original, unhashed key
					c.store.Del(victim.keyHash, nil)
				}
			case itemUpdate:
				c.policy.Update(i.keyHash, i.cost)
			case itemDelete:
				c.policy.Del(i.keyHash)
				c.store.Del(i.keyHash, i.key)
			}
		case <-c.stop:
			return
		}
	}
}

// collectMetrics just creates a new *Metrics instance and adds the pointers
// to the cache and policy instances.
func (c *Cache) collectMetrics() {
	c.Metrics = newMetrics()
	c.policy.CollectMetrics(c.Metrics)
}

type metricType int

const (
	// The following 2 keep track of hits and misses.
	hit = iota
	miss
	// The following 3 keep track of number of keys added, updated and evicted.
	keyAdd
	keyUpdate
	keyEvict
	// The following 2 keep track of cost of keys added and evicted.
	costAdd
	costEvict
	// The following keep track of how many sets were dropped or rejected later.
	dropSets
	rejectSets
	// The following 2 keep track of how many gets were kept and dropped on the
	// floor.
	dropGets
	keepGets
	// This should be the final enum. Other enums should be set before this.
	doNotUse
)

func stringFor(t metricType) string {
	switch t {
	case hit:
		return "hit"
	case miss:
		return "miss"
	case keyAdd:
		return "keys-added"
	case keyUpdate:
		return "keys-updated"
	case keyEvict:
		return "keys-evicted"
	case costAdd:
		return "cost-added"
	case costEvict:
		return "cost-evicted"
	case dropSets:
		return "sets-dropped"
	case rejectSets:
		return "sets-rejected" // by policy.
	case dropGets:
		return "gets-dropped"
	case keepGets:
		return "gets-kept"
	default:
		return "unidentified"
	}
}

// Metrics is a snapshot of performance statistics for the lifetime of a cache
// instance.
type Metrics struct {
	all [doNotUse][]*uint64
}

func newMetrics() *Metrics {
	s := &Metrics{}
	for i := 0; i < doNotUse; i++ {
		s.all[i] = make([]*uint64, 256)
		slice := s.all[i]
		for j := range slice {
			slice[j] = new(uint64)
		}
	}
	return s
}

func (p *Metrics) add(t metricType, hash, delta uint64) {
	if p == nil {
		return
	}
	valp := p.all[t]
	// Avoid false sharing by padding at least 64 bytes of space between two
	// atomic counters which would be incremented.
	idx := (hash % 25) * 10
	atomic.AddUint64(valp[idx], delta)
}

func (p *Metrics) get(t metricType) uint64 {
	if p == nil {
		return 0
	}
	valp := p.all[t]
	var total uint64
	for i := range valp {
		total += atomic.LoadUint64(valp[i])
	}
	return total
}

// Hits is the number of Get calls where a value was found for the corresponding
// key.
func (p *Metrics) Hits() uint64 {
	return p.get(hit)
}

// Misses is the number of Get calls where a value was not found for the
// corresponding key.
func (p *Metrics) Misses() uint64 {
	return p.get(miss)
}

// KeysAdded is the total number of Set calls where a new key-value item was
// added.
func (p *Metrics) KeysAdded() uint64 {
	return p.get(keyAdd)
}

// KeysUpdated is the total number of Set calls where the value was updated.
func (p *Metrics) KeysUpdated() uint64 {
	return p.get(keyUpdate)
}

// KeysEvicted is the total number of keys evicted.
func (p *Metrics) KeysEvicted() uint64 {
	return p.get(keyEvict)
}

// CostAdded is the sum of costs that have been added (successful Set calls).
func (p *Metrics) CostAdded() uint64 {
	return p.get(costAdd)
}

// CostEvicted is the sum of all costs that have been evicted.
func (p *Metrics) CostEvicted() uint64 {
	return p.get(costEvict)
}

// SetsDropped is the number of Set calls that don't make it into internal
// buffers (due to contention or some other reason).
func (p *Metrics) SetsDropped() uint64 {
	return p.get(dropSets)
}

// SetsRejected is the number of Set calls rejected by the policy (TinyLFU).
func (p *Metrics) SetsRejected() uint64 {
	return p.get(rejectSets)
}

// GetsDropped is the number of Get counter increments that are dropped
// internally.
func (p *Metrics) GetsDropped() uint64 {
	return p.get(dropGets)
}

// GetsKept is the number of Get counter increments that are kept.
func (p *Metrics) GetsKept() uint64 {
	return p.get(keepGets)
}

// Ratio is the number of Hits over all accesses (Hits + Misses). This is the
// percentage of successful Get calls.
func (p *Metrics) Ratio() float64 {
	if p == nil {
		return 0.0
	}
	hits, misses := p.get(hit), p.get(miss)
	if hits == 0 && misses == 0 {
		return 0.0
	}
	return float64(hits) / float64(hits+misses)
}

func (p *Metrics) Clear() {
	if p == nil {
		return
	}
	for i := 0; i < doNotUse; i++ {
		for j := range p.all[i] {
			atomic.StoreUint64(p.all[i][j], 0)
		}
	}
}

func (p *Metrics) String() string {
	if p == nil {
		return ""
	}
	var buf bytes.Buffer
	for i := 0; i < doNotUse; i++ {
		t := metricType(i)
		fmt.Fprintf(&buf, "%s: %d ", stringFor(t), p.get(t))
	}
	fmt.Fprintf(&buf, "gets-total: %d ", p.get(hit)+p.get(miss))
	fmt.Fprintf(&buf, "hit-ratio: %.2f", p.Ratio())
	return buf.String()
}
