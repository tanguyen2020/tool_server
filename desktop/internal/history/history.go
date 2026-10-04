// Package history persists metrics on disk (bbolt) so charts can show hours or days,
// and survive app restarts. Samples are averaged into 1-minute records:
//
//	h:<server>  minute -> 15 float32 host values (NaN = missing)
//	d:<server>  minute -> uint16 count, then (uint16 name id, 6 float32: cpu, mem, net rx, net tx, disk read, disk write)
//	c:<server>  older records with cpu and mem only (still read, no longer written)
//	e:<server>  unix ms + 4-byte hash -> JSON container event
//	n:<server>  "n"+name -> uint16 id, "i"+id -> name
//
// Gaps (the app was closed, the server was down) stay empty and show as breaks in the charts.
package history

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/fnv"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"serverdash/internal/monitor"
)

const (
	Resolution = time.Minute
	Retention  = 7 * 24 * time.Hour
	MaxPoints  = 720 // points returned per series, whatever the range
	nHost      = 15
	nCtr       = 6 // cpu, mem, net rx, net tx, disk read, disk write (bytes/s)
	ctrEntry   = 2 + nCtr*4
	maxEvents  = 2000
)

// host value order; must match hostValues and Point below.
func hostValues(p monitor.Point) [nHost]float64 {
	v := func(f *float64) float64 {
		if f == nil {
			return math.NaN()
		}
		return *f
	}
	return [nHost]float64{
		v(p.CPU), p.Mem, v(p.RX), v(p.TX), v(p.User), v(p.System), v(p.IOWait), v(p.Steal),
		p.Load1, p.Load5, p.Load15, p.MemUsed, p.MemCache, v(p.IORead), v(p.IOWrite),
	}
}

// Point is one bucket of host history; nil = no data in that bucket.
type Point struct {
	T        int64    `json:"t"`
	CPU      *float64 `json:"cpu"`
	Mem      *float64 `json:"mem"`
	RX       *float64 `json:"rx"`
	TX       *float64 `json:"tx"`
	User     *float64 `json:"user"`
	System   *float64 `json:"system"`
	IOWait   *float64 `json:"iowait"`
	Steal    *float64 `json:"steal"`
	Load1    *float64 `json:"load1"`
	Load5    *float64 `json:"load5"`
	Load15   *float64 `json:"load15"`
	MemUsed  *float64 `json:"memUsed"`
	MemCache *float64 `json:"memCache"`
	IORead   *float64 `json:"ioRead"`
	IOWrite  *float64 `json:"ioWrite"`
}

func (p *Point) fields() [nHost]**float64 {
	return [nHost]**float64{
		&p.CPU, &p.Mem, &p.RX, &p.TX, &p.User, &p.System, &p.IOWait, &p.Steal,
		&p.Load1, &p.Load5, &p.Load15, &p.MemUsed, &p.MemCache, &p.IORead, &p.IOWrite,
	}
}

// Result is the answer to a range query. Container series are aligned with Times.
type Result struct {
	Step     int64                 `json:"step"` // bucket size in ms
	Host     []Point               `json:"host"`
	Times    []int64               `json:"times"`
	CPU      map[string][]*float64 `json:"cpu"`
	Mem      map[string][]*float64 `json:"mem"`
	NetRx    map[string][]*float64 `json:"netRx"`
	NetTx    map[string][]*float64 `json:"netTx"`
	BlkRead  map[string][]*float64 `json:"blkRead"`
	BlkWrite map[string][]*float64 `json:"blkWrite"`
}

type agg struct {
	sum float64
	n   int
}

func (a *agg) add(v float64) {
	if !math.IsNaN(v) {
		a.sum += v
		a.n++
	}
}

func (a agg) avg() float64 {
	if a.n == 0 {
		return math.NaN()
	}
	return a.sum / float64(a.n)
}

// minute accumulates the samples of the current minute for one server.
type minute struct {
	start int64 // unix ms of the minute
	host  [nHost]agg
	ctr   map[string]*[nCtr]agg
}

type Store struct {
	db    *bolt.DB
	mu    sync.Mutex
	open  map[string]*minute
	names map[string]map[string]uint16 // server -> name -> id (cache of the n: bucket)
}

func Open(dir string) (*Store, error) {
	db, err := bolt.Open(filepath.Join(dir, "history.db"), 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, open: map[string]*minute{}, names: map[string]map[string]uint16{}}
	go s.pruneLoop()
	return s, nil
}

// Close writes the minutes in progress and closes the database.
func (s *Store) Close() error {
	s.mu.Lock()
	pending := s.open
	s.open = map[string]*minute{}
	s.mu.Unlock()
	for id, m := range pending {
		_ = s.write(id, m)
	}
	return s.db.Close()
}

// Add records one online snapshot.
func (s *Store) Add(snap *monitor.Snapshot) {
	if snap.Status != "online" || snap.Host == nil || snap.Host.CPU == nil {
		return
	}
	start := snap.UpdatedAt - snap.UpdatedAt%Resolution.Milliseconds()
	s.mu.Lock()
	m := s.open[snap.ID]
	var done *minute
	if m == nil || m.start != start {
		done = m
		m = &minute{start: start, ctr: map[string]*[nCtr]agg{}}
		s.open[snap.ID] = m
	}
	vals := hostValues(monitor.PointOf(snap))
	for i, v := range vals {
		m.host[i].add(v)
	}
	if snap.Docker != nil {
		for _, c := range snap.Docker.Containers {
			vals := [nCtr]*float64{c.CPU, c.MemUsed, c.NetRx, c.NetTx, c.BlkRead, c.BlkWrite}
			if vals[0] == nil && vals[1] == nil {
				continue
			}
			a := m.ctr[c.Name]
			if a == nil {
				a = &[nCtr]agg{}
				m.ctr[c.Name] = a
			}
			for i, v := range vals {
				if v != nil {
					a[i].add(*v)
				}
			}
		}
	}
	s.mu.Unlock()
	if done != nil {
		go func() { _ = s.write(snap.ID, done) }()
	}
}

func key(ms int64) []byte {
	k := make([]byte, 8)
	binary.BigEndian.PutUint64(k, uint64(ms))
	return k
}

func putF32(b []byte, v float64) {
	binary.LittleEndian.PutUint32(b, math.Float32bits(float32(v)))
}

func getF32(b []byte) float64 {
	return float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
}

// nameID returns the stable id of a container name, creating it inside tx when needed.
func (s *Store) nameID(tx *bolt.Tx, server, name string) (uint16, error) {
	s.mu.Lock()
	cache := s.names[server]
	if cache == nil {
		cache = map[string]uint16{}
		s.names[server] = cache
	}
	id, ok := cache[name]
	s.mu.Unlock()
	if ok {
		return id, nil
	}
	b, err := tx.CreateBucketIfNotExists([]byte("n:" + server))
	if err != nil {
		return 0, err
	}
	if v := b.Get([]byte("n" + name)); v != nil {
		id = binary.LittleEndian.Uint16(v)
	} else {
		seq, err := b.NextSequence()
		if err != nil {
			return 0, err
		}
		if seq > math.MaxUint16 {
			return 0, errors.New("too many container names")
		}
		id = uint16(seq)
		raw := make([]byte, 2)
		binary.LittleEndian.PutUint16(raw, id)
		if err := b.Put([]byte("n"+name), raw); err != nil {
			return 0, err
		}
		if err := b.Put(append([]byte("i"), raw...), []byte(name)); err != nil {
			return 0, err
		}
	}
	s.mu.Lock()
	cache[name] = id
	s.mu.Unlock()
	return id, nil
}

func (s *Store) write(server string, m *minute) error {
	return s.db.Batch(func(tx *bolt.Tx) error {
		hb, err := tx.CreateBucketIfNotExists([]byte("h:" + server))
		if err != nil {
			return err
		}
		host := make([]byte, nHost*4)
		for i, a := range m.host {
			putF32(host[i*4:], a.avg())
		}
		if err := hb.Put(key(m.start), host); err != nil {
			return err
		}
		if len(m.ctr) == 0 {
			return nil
		}
		cb, err := tx.CreateBucketIfNotExists([]byte("d:" + server))
		if err != nil {
			return err
		}
		rec := make([]byte, 2, 2+len(m.ctr)*ctrEntry)
		binary.LittleEndian.PutUint16(rec, uint16(len(m.ctr)))
		for name, a := range m.ctr {
			id, err := s.nameID(tx, server, name)
			if err != nil {
				return err
			}
			e := make([]byte, ctrEntry)
			binary.LittleEndian.PutUint16(e, id)
			for i := range a {
				putF32(e[2+i*4:], a[i].avg())
			}
			rec = append(rec, e...)
		}
		return cb.Put(key(m.start), rec)
	})
}

// Query returns at most MaxPoints buckets covering [from, to).
func (s *Store) Query(server string, from, to time.Time) (*Result, error) {
	span := to.Sub(from)
	step := time.Duration(math.Ceil(float64(span)/float64(MaxPoints)/float64(Resolution))) * Resolution
	if step < Resolution {
		step = Resolution
	}
	f0 := from.UnixMilli() - from.UnixMilli()%step.Milliseconds()
	n := int((to.UnixMilli()-f0)/step.Milliseconds()) + 1
	stepMs := step.Milliseconds()

	host := make([][nHost]agg, n)
	ctr := map[uint16]*[nCtr][]agg{}
	idName := map[uint16]string{}

	err := s.db.View(func(tx *bolt.Tx) error {
		if hb := tx.Bucket([]byte("h:" + server)); hb != nil {
			c := hb.Cursor()
			for k, v := c.Seek(key(f0)); k != nil && int64(binary.BigEndian.Uint64(k)) < to.UnixMilli(); k, v = c.Next() {
				i := int((int64(binary.BigEndian.Uint64(k)) - f0) / stepMs)
				if i < 0 || i >= n || len(v) < nHost*4 {
					continue
				}
				for j := 0; j < nHost; j++ {
					host[i][j].add(getF32(v[j*4:]))
				}
			}
		}
		// Older "c:" records hold cpu and mem only; "d:" records hold all six values.
		for _, layout := range []struct {
			prefix string
			fields int
		}{{"c:", 2}, {"d:", nCtr}} {
			cb := tx.Bucket([]byte(layout.prefix + server))
			if cb == nil {
				continue
			}
			size := 2 + layout.fields*4
			c := cb.Cursor()
			for k, v := c.Seek(key(f0)); k != nil && int64(binary.BigEndian.Uint64(k)) < to.UnixMilli(); k, v = c.Next() {
				i := int((int64(binary.BigEndian.Uint64(k)) - f0) / stepMs)
				if i < 0 || i >= n || len(v) < 2 {
					continue
				}
				count := int(binary.LittleEndian.Uint16(v))
				for e := 0; e < count && 2+(e+1)*size <= len(v); e++ {
					rec := v[2+e*size:]
					id := binary.LittleEndian.Uint16(rec)
					a := ctr[id]
					if a == nil {
						a = &[nCtr][]agg{}
						for j := range a {
							a[j] = make([]agg, n)
						}
						ctr[id] = a
					}
					for j := 0; j < layout.fields; j++ {
						a[j][i].add(getF32(rec[2+j*4:]))
					}
				}
			}
		}
		if nb := tx.Bucket([]byte("n:" + server)); nb != nil {
			raw := make([]byte, 2)
			for id := range ctr {
				binary.LittleEndian.PutUint16(raw, id)
				if name := nb.Get(append([]byte("i"), raw...)); name != nil {
					idName[id] = string(name)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	res := &Result{Step: stepMs, Host: make([]Point, n), Times: make([]int64, n),
		CPU: map[string][]*float64{}, Mem: map[string][]*float64{}, NetRx: map[string][]*float64{}, NetTx: map[string][]*float64{},
		BlkRead: map[string][]*float64{}, BlkWrite: map[string][]*float64{}}
	outs := [nCtr]map[string][]*float64{res.CPU, res.Mem, res.NetRx, res.NetTx, res.BlkRead, res.BlkWrite}
	ptr := func(a agg) *float64 {
		if a.n == 0 {
			return nil
		}
		v := a.avg()
		return &v
	}
	for i := 0; i < n; i++ {
		t := f0 + int64(i)*stepMs
		res.Times[i] = t
		res.Host[i].T = t
		for j, f := range res.Host[i].fields() {
			*f = ptr(host[i][j])
		}
	}
	for id, a := range ctr {
		name, ok := idName[id]
		if !ok {
			continue
		}
		for j := range a {
			series := make([]*float64, n)
			hasData := false
			for i := 0; i < n; i++ {
				series[i] = ptr(a[j][i])
				hasData = hasData || series[i] != nil
			}
			if hasData {
				outs[j][name] = series
			}
		}
	}
	return res, nil
}

// DeleteServer removes everything recorded for a server.
func (s *Store) DeleteServer(server string) error {
	s.mu.Lock()
	delete(s.open, server)
	delete(s.names, server)
	s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, p := range []string{"h:", "c:", "d:", "e:", "n:"} {
			if err := tx.DeleteBucket([]byte(p + server)); err != nil && !errors.Is(err, bolt.ErrBucketNotFound) {
				return err
			}
		}
		return nil
	})
}

// Prune deletes records older than the retention period.
func (s *Store) Prune(now time.Time) error {
	limit := key(now.Add(-Retention).UnixMilli())
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			if len(name) < 2 || !strings.ContainsRune("hcde", rune(name[0])) || name[1] != ':' {
				return nil
			}
			c := b.Cursor()
			for k, _ := c.First(); k != nil && string(k) < string(limit); k, _ = c.First() {
				if err := c.Delete(); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func (s *Store) pruneLoop() {
	for {
		_ = s.Prune(time.Now())
		time.Sleep(time.Hour)
	}
}

// AddEvents stores container events. Keys include a hash of the event, so storing the same event twice is harmless.
func (s *Store) AddEvents(server string, events []monitor.Event) error {
	return s.db.Batch(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("e:" + server))
		if err != nil {
			return err
		}
		for _, e := range events {
			h := fnv.New32a()
			h.Write([]byte(e.Container + "|" + e.Action + "|" + e.Detail))
			k := append(key(e.T), h.Sum(nil)...)
			v, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := b.Put(k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// Events returns the container events in [from, to), oldest first, at most maxEvents (the most recent ones).
func (s *Store) Events(server string, from, to time.Time) ([]monitor.Event, error) {
	out := []monitor.Event{}
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("e:" + server))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Seek(key(from.UnixMilli())); k != nil && int64(binary.BigEndian.Uint64(k[:8])) < to.UnixMilli(); k, v = c.Next() {
			var e monitor.Event
			if json.Unmarshal(v, &e) == nil {
				out = append(out, e)
			}
		}
		return nil
	})
	if len(out) > maxEvents {
		out = out[len(out)-maxEvents:]
	}
	return out, err
}
