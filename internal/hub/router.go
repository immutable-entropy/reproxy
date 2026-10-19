package hub

import (
	"context"
	"sort"
	"sync"
	"time"

	"reproxy/internal/instance"
)

// Router finds instances by ID: the local one directly, peers over HTTP.
type Router struct {
	local *instance.Local
	token func() (string, error)

	mu    sync.Mutex
	peers map[string]*peer
}

type peer struct {
	info     instance.Info
	addr     string
	remote   *instance.Remote
	lastSeen time.Time
}

func newRouter(local *instance.Local, token func() (string, error)) *Router {
	return &Router{local: local, token: token, peers: make(map[string]*peer)}
}

// LocalID returns this process's instance ID.
func (r *Router) LocalID() string { return r.local.ID() }

// Get returns the instance with the given ID.
func (r *Router) Get(id string) (instance.Instance, bool) {
	if id == r.local.ID() {
		return r.local, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[id]
	if !ok {
		return nil, false
	}
	return p.remote, true
}

// Instances describes every reachable instance, this one first and then
// peers in the order they started. Peers that fail to answer are skipped.
func (r *Router) Instances(ctx context.Context) []instance.Info {
	self, _ := r.local.Info(ctx)
	out := []instance.Info{self}

	r.mu.Lock()
	peers := make([]*peer, 0, len(r.peers))
	for _, p := range r.peers {
		peers = append(peers, p)
	}
	r.mu.Unlock()

	infos := make([]instance.Info, len(peers))
	ok := make([]bool, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if info, err := p.remote.Info(cctx); err == nil {
				infos[i], ok[i] = info, true
			}
		}()
	}
	wg.Wait()
	live := make([]instance.Info, 0, len(peers))
	for i := range peers {
		if ok[i] {
			live = append(live, infos[i])
		}
	}
	sort.Slice(live, func(a, b int) bool { return live[a].Started.Before(live[b].Started) })
	return append(out, live...)
}

// Peers returns the IDs of the registered peers.
func (r *Router) Peers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.peers))
	for id := range r.peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *Router) register(info instance.Info, addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if info.ID == r.local.ID() {
		return
	}
	p, ok := r.peers[info.ID]
	if !ok || p.addr != addr {
		p = &peer{addr: addr, remote: instance.NewRemote(addr, r.token)}
		r.peers[info.ID] = p
	}
	p.info, p.lastSeen = info, time.Now()
}

func (r *Router) prune(ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.peers {
		if time.Since(p.lastSeen) > ttl {
			delete(r.peers, id)
		}
	}
}
